package api

// Team request lineage over REST: attaching a request to the work that serves it,
// and planning a batch of tickets for it (epic 202, story 549, AMP #3974; build
// slice F5 of discoveries/team-inbox-approved-build-slices.md; approved product
// contract artifact 36 v2 §2 and §3).
//
//	POST /api/teams/:id/requests/:request/links     one relation (routes_to / planned_as / delivered_by)
//	POST /api/teams/:id/requests/:request/handoff   a batch of plan items, created and linked
//
// Both are thin adapters over the one reviewed write path each already has —
// internal/service/request_links.go's AttachRequest (E1's attach half, #3962) and
// internal/service/request_handoff.go's HandoffRequest (E1's handoff half, #3965) —
// which are the same two methods the MCP tools call (internal/mcp/request_planning.go,
// slice I3). They decide nothing: who may attach or plan, whether the request is
// routed, whether a replay writes anything, how a partial batch reconciles, which
// pair of kinds the contract allows, and every audit and history row are the
// service's, so a client's codes and statuses are the same on either surface.
//
// # What these routes own, and nothing else
//
// The transport facts the service cannot know:
//
//   - **The path shape is the writes' and the reads'.** A write names its team as
//     well as its request, because the service's every request write loads the team
//     first (its row carries the org half of the tenant), and the `:request` segment
//     is the serial — a public reference on a write path is refused by name rather
//     than searched for (#3971's rule, kept here through the shared
//     `requestLifecycleTarget`).
//   - **A bounded, strict body.** `decodeRequestMutation` (#3971's) is reused for
//     both routes: unknown fields are refused rather than dropped, an over-bound
//     body is refused without being read, and a second JSON value is refused. A
//     payload naming a decision a handoff must not make (`assigned_to`, `start_at`,
//     `approve`, …) is answered with the rule instead of the decoder's generic
//     "unknown field", because a caller that sent one believes it scheduled or
//     approved something.
//   - **The answer is the act, not the record.** A link answers with the stored
//     relation, whether this call wrote it, and the target it resolved; a handoff
//     answers with the batch's ref→ticket mapping and the counts a retry needs
//     ("2 created, 0 already there"). Neither answers with a request record, a
//     ticket record or an approval state.
//
// # Retry is the contract, and it is the service's
//
// `changed=false` (attach) and `created=0` (handoff) are the two honest ways a
// replay answers. A replayed attach writes no row, no history entry and no audit
// entry; a handoff retried with its own `handoff_key` plans only what is missing and
// reports the rest as `already`. Neither route invents an idempotency key: the
// attach's key is the relation itself (055's unique key) and the handoff's is the
// caller's `handoff_key`, which is why that field is required rather than defaulted
// here.
//
// # What is deliberately NOT here
//
//   - **No approval and no dispatch.** A handoff creates tickets that await a
//     person's approval; nothing in this file calls ApproveTickets, ApprovePlan or
//     Dispatch, and the created tickets carry no `approved_at`, no `dispatch_id` and
//     no schedule. The tests drive the real dispatch gate rather than trusting this
//     sentence.
//   - **No detach, no unlink.** A planning detachment and a duplicate's detach are
//     deliberate acts with their own rules (#3962's header); adding a verb here
//     would put a second policy in one file.
//   - **No attachment route.** Files are request_uploads.go's; the download mint
//     stays request_download.go's (#3963).
//   - **No revision check.** A link is lineage, not a versioned edit of the
//     request's brief, so attaching does not invalidate a client's copy of it and
//     these routes take no `If-Match`. The handoff writes the request's
//     `planned_at` through the link repository's own rule, not a CAS.
//   - **No remote fetch of any kind.** Nothing here opens a connection to anything
//     a caller names; a link names an id in AMP's own tables and the service
//     resolves its tenant from that row.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// maxRequestHandoffItems is the most plan items one handoff may carry, and it is
// Service.CreateTasks' own bound restated here for one reason: an over-size batch is
// then a coded `request.invalid` naming the number instead of an uncoded 400 from
// deep inside the planning path. A batch over it cannot be planned by any caller, so
// this refuses nothing the service would have allowed.
const maxRequestHandoffItems = 100

// requestLinkBody is what an attach may send: the relation, what it points at, and
// the one flag a second route needs.
//
// There is deliberately no org, no actor, no resolved target org, no "the target
// exists" claim and no revision: the tenant comes from the path's team row, the
// target's real tenant and the caller's access to it are read from the target's own
// record, and a link is lineage rather than a versioned edit. A payload naming one
// of those is refused by the decoder rather than dropped.
type requestLinkBody struct {
	// LinkKind and TargetKind are passed through unnormalized: the domain owns the
	// contract's matrix and refuses a pair it never allows, so there is one
	// implementation of "which relation may point at what" and this file cannot
	// drift from it.
	LinkKind   string `json:"link_kind"`
	TargetKind string `json:"target_kind"`
	// TargetID names the project, epic, story, ticket, artifact or receipt. The
	// service refuses a non-positive id by name before anything is loaded.
	TargetID int `json:"target_id"`
	// SupersedeRoute allows a `routes_to` attach to move the request to a different
	// project than its active route (D9). Without it such an attach is a 409 naming
	// the current project; superseding appends the new routing and keeps the old one
	// as history.
	SupersedeRoute bool `json:"supersede_route"`
}

// requestLinkTargetFacts is what a link's target resolved to: its kind and id, and
// the project it lives in — read from the target's own row, never from the caller.
// It is absent on a replay of a relation whose target has since been deleted, which
// is the tombstone the relation itself still records.
type requestLinkTargetFacts struct {
	Kind      domain.RequestTargetKind `json:"kind"`
	ID        int                      `json:"id"`
	ProjectID int                      `json:"project_id"`
}

// requestLinkAnswer is the answer to one attach: the stored relation, whether this
// call wrote it, and the target it resolved.
//
// `changed=false` is a replay: nothing was written at all — no second row, no second
// history entry, no second audit entry — and the relation answered with is the
// original row with its first attribution intact.
type requestLinkAnswer struct {
	Link    *domain.RequestLink     `json:"link"`
	Changed bool                    `json:"changed"`
	Target  *requestLinkTargetFacts `json:"target,omitempty"`
}

// requestHandoffBody is one planning handoff: the caller's retry token and the batch.
//
// There is deliberately no project, no org, no actor and no decision-shaped field:
// the project is the request's active route (D9) and the org is the team's, both read
// from rows. A payload naming one of them is refused by name (see
// handoffForbiddenFields) rather than dropped, because a dropped `project_id` leaves
// the caller believing it chose where the work was planned.
type requestHandoffBody struct {
	// HandoffKey is the caller's idempotency token for THIS batch: the same key
	// retries, a new key is a deliberate re-plan. The domain normalizes and validates
	// it; this file only requires that one was sent, so a missing key is a 400 naming
	// the field rather than a batch that silently cannot be retried.
	HandoffKey string               `json:"handoff_key"`
	Items      []requestHandoffItem `json:"items"`
}

// requestHandoffItem is one ticket the caller wants planned.
//
// Every field is a PLAN decision the caller is entitled to make, and the ones it is
// not entitled to make have no field here at all: no assignee, no owner, no
// `start_at`, no performer, no `decide_by`, no metadata and no approval. That is the
// structural half of "request text decides nothing"; the adapter's half is
// handoffForbiddenFields, so a caller that sends one is told why.
type requestHandoffItem struct {
	// Ref names this item within the batch, so another item can depend on it and so a
	// retry can tell "this one already exists" from "this one is new". It is the
	// ledger's key, so it is stable across attempts of the same handoff.
	Ref string `json:"ref"`
	// StoryID is where the ticket goes; it must be in the request's routed project,
	// which CreateTasks checks.
	StoryID int `json:"story_id"`
	// Name is the ticket's one-line title — a label, not an instruction. The
	// requester's own words belong in Plan's quote, never in the name.
	Name string `json:"name"`
	// Plan is the plan author's instruction, written outside the untrusted fence.
	// The request is appended inside it by domain.HandoffBrief.
	Plan string `json:"plan"`
	// AcceptanceCriteria is the plan author's definition of done.
	AcceptanceCriteria string `json:"acceptance_criteria"`
	// Priority is the plan's severity (0 low, 1 normal, 2 high, 3 critical).
	Priority string `json:"priority"`
	// DependsOn names what this item waits on: refs in this batch, or existing ticket
	// ids. Both a string ("api", "#123", "123") and a JSON number (123) are accepted,
	// because a REST client naturally writes an id as a number and the MCP reader
	// accepts one too — refusing it here would make the two surfaces disagree about
	// the same batch. Anything else is refused rather than dropped: a silently dropped
	// dependency is a ticket that starts before the work it needs.
	DependsOn []json.RawMessage `json:"depends_on"`
}

// requestHandoffAnswer is one handoff attempt: where the batch was planned, what
// each ref became, and the counts a retry reads.
//
// `created` and `already` are next to the items on purpose: "2 created, 0 already
// there" is the truthful report of a first attempt and "0 created, 2 already there"
// the truthful report of a retry, and a client should not have to count the list to
// know which it got.
type requestHandoffAnswer struct {
	ProjectID int                         `json:"project_id"`
	Items     []service.HandoffItemResult `json:"items"`
	Created   int                         `json:"created"`
	Already   int                         `json:"already"`
	Warnings  []string                    `json:"warnings,omitempty"`
}

// handoffNoDecisionRule is what every refusal of a plan-shaped-but-forbidden field
// says. It is one sentence because there is one rule (contract §3): planning decides
// what serves a request, never who does it, when it runs or whether it is approved.
const handoffNoDecisionRule = "planning a request decides what work serves it, never who does it, when it runs, or whether it is " +
	"approved: the ticket's owner is the triager doing the planning, a person approves it afterwards in the web UI, and the " +
	"request's own words cannot set any of those"

// handoffForbiddenFields are the body fields a handoff must not be able to set, and
// the sentence that explains why. They are checked off the decoder's unknown-field
// refusal so the caller gets the rule rather than "unknown field".
//
// The list mirrors internal/mcp/request_planning.go's two negative lists (batch level
// and item level) because a caller must not be able to do over REST what a tool
// refuses over MCP. A field a caller might send at either level lands here: the JSON
// decoder names the field without its path, so one map answers both.
//
// `story_id` is in the map for the opposite reason to the rest: it is a legitimate
// field of an ITEM, so a batch that carries one at the top level is a caller who put
// the batch's shape one level too high, and saying so is more useful than the
// decoder's generic message.
var handoffForbiddenFields = map[string]string{
	"project_id": handoffNoDecisionRule, "epic_id": handoffNoDecisionRule,
	"org_id":      "the inbox and its org are the path's, and the project a batch is planned into is the request's own route (D9): nothing a handoff sends may choose a tenant",
	"story_id":    "story_id belongs to one item, not to the batch: send items[].story_id, and the item is placed in the project the request is routed to",
	"assigned_to": handoffNoDecisionRule, "assignee": handoffNoDecisionRule,
	"agent_id": handoffNoDecisionRule, "owner": handoffNoDecisionRule, "owner_user_id": handoffNoDecisionRule,
	"start_at": handoffNoDecisionRule, "schedule": handoffNoDecisionRule,
	"decide_by": handoffNoDecisionRule, "ask": handoffNoDecisionRule,
	"performer": handoffNoDecisionRule, "human_reason": handoffNoDecisionRule,
	"metadata": handoffNoDecisionRule,
	"approve":  handoffNoDecisionRule, "approved": handoffNoDecisionRule, "approval": handoffNoDecisionRule,
	"dispatch": handoffNoDecisionRule, "dispatch_id": handoffNoDecisionRule,
	"actor":             "the actor is the authenticated caller, read from the principal: a payload cannot name who is planning",
	"actor_user_id":     "the actor is the authenticated caller, read from the principal: a payload cannot name who is planning",
	"user_id":           "the actor is the authenticated caller, read from the principal: a payload cannot name who is planning",
	"requester":         "the requester is the request's own row, never a field of the plan that serves it",
	"requester_user_id": "the requester is the request's own row, never a field of the plan that serves it",
	"status":            handoffNoDecisionRule, "state": handoffNoDecisionRule, "task_id": handoffNoDecisionRule,
}

// ---- The two routes ----

// teamRequestLink serves POST /api/teams/:id/requests/:request/links: one relation
// between the request and the work that serves it.
//
// 201 when this call wrote the relation, 200 when it answered a replay with the row
// that already existed — the same split the submission route uses for a create
// versus a replay, so a client can tell the two apart without reading the body.
func (h *RestHandler) teamRequestLink(w http.ResponseWriter, r *http.Request) {
	teamID, requestID, ok := requestLifecycleTarget(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var body requestLinkBody
	if err := decodeRequestMutation(w, r, &body, "request link"); err != nil {
		fail(w, err, http.StatusBadRequest)
		return
	}
	if body.SupersedeRoute && !strings.EqualFold(strings.TrimSpace(body.TargetKind), string(domain.RequestTargetProject)) {
		// A route flag on a relation that has no route is a caller who believes it is
		// moving something. Answering is cheaper than a silent no-op, and the answer
		// teaches the rule (the MCP adapter's reasoning, kept identical).
		fail(w, authz.RequestInvalid("supersede_route only applies to a routes_to link to a project: a %s link names its own "+
			"relation and has no route to move", body.LinkKind), http.StatusBadRequest)
		return
	}
	res, err := h.svc.AttachRequest(r.Context(), p, service.AttachRequestInput{
		RequestID: requestID,
		TeamID:    teamID,
		// The pair is validated by the domain before anything is loaded, so it is
		// handed over as written rather than pre-interpreted here.
		LinkKind:       domain.RequestLinkKind(body.LinkKind),
		TargetKind:     domain.RequestTargetKind(body.TargetKind),
		TargetID:       body.TargetID,
		SupersedeRoute: body.SupersedeRoute,
	})
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	if res == nil || res.Link == nil {
		// Unreachable through the service, which returns the stored row on every
		// success. Failing closed keeps a future caller from answering 201 for an act
		// with nothing to show for itself.
		fail(w, errorf("the link write answered no relation"), http.StatusInternalServerError)
		return
	}
	out := requestLinkAnswer{Link: res.Link, Changed: res.Changed}
	if res.Target != nil {
		out.Target = &requestLinkTargetFacts{Kind: res.Target.Kind, ID: res.Target.ID, ProjectID: res.Target.ProjectID}
	}
	if res.Changed {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
	}
	jsonOK(w, out)
}

// teamRequestHandoff serves POST /api/teams/:id/requests/:request/handoff: a batch of
// plan items, created in the project the request is routed to and linked to it as
// planned_as.
//
// It calls HandoffRequest rather than CreateTasks + AttachRequest because the retry
// ledger, the untrusted-quote wrapper and the "cannot decide who or when" refusals
// live there: one implementation, checked by this route and by the MCP tool alike.
func (h *RestHandler) teamRequestHandoff(w http.ResponseWriter, r *http.Request) {
	teamID, requestID, ok := requestLifecycleTarget(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	body, ok := decodeRequestHandoff(w, r)
	if !ok {
		return
	}
	items := make([]service.HandoffItem, 0, len(body.Items))
	for i, it := range body.Items {
		deps, err := handoffDependsOn(it.DependsOn, i)
		if err != nil {
			fail(w, err, http.StatusBadRequest)
			return
		}
		items = append(items, service.HandoffItem{
			Ref:                it.Ref,
			StoryID:            it.StoryID,
			Name:               it.Name,
			Plan:               it.Plan,
			AcceptanceCriteria: it.AcceptanceCriteria,
			Priority:           it.Priority,
			DependsOn:          deps,
		})
	}
	res, err := h.svc.HandoffRequest(r.Context(), p, service.HandoffRequestInput{
		RequestID:  requestID,
		TeamID:     teamID,
		HandoffKey: body.HandoffKey,
		Items:      items,
	})
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	if res == nil {
		fail(w, errorf("the handoff answered no batch"), http.StatusInternalServerError)
		return
	}
	created := 0
	for _, it := range res.Items {
		if it.Created {
			created++
		}
	}
	jsonOK(w, requestHandoffAnswer{
		ProjectID: res.ProjectID,
		Items:     res.Items,
		Created:   created,
		Already:   len(res.Items) - created,
		Warnings:  res.Warnings,
	})
}

// ---- Decoding ----

// decodeRequestHandoff reads one handoff body: bounded and strict like every other
// request write, with the plan's forbidden decisions answered by name.
//
// The bound and the two structural refusals (unknown field, more than one JSON value)
// are `decodeRequestMutation`'s. What this function adds is the mapping from the
// decoder's unknown-field message onto the handoff rule, and the item ceiling, which
// CreateTasks owns but cannot code: a batch over it would otherwise reach the client
// as an uncoded 400 from inside the planning path.
func decodeRequestHandoff(w http.ResponseWriter, r *http.Request) (requestHandoffBody, bool) {
	var body requestHandoffBody
	if err := decodeRequestMutation(w, r, &body, "request handoff"); err != nil {
		if field := unknownField(err); field != "" {
			if why, forbidden := handoffForbiddenFields[field]; forbidden {
				fail(w, authz.RequestInvalid("%q is not a field of a handoff: %s", field, why), http.StatusBadRequest)
				return body, false
			}
		}
		fail(w, err, http.StatusBadRequest)
		return body, false
	}
	if len(body.Items) > maxRequestHandoffItems {
		fail(w, authz.RequestInvalid("items holds %d plan items, and a handoff plans at most %d at a time: split the batch and send "+
			"a handoff_key per batch (a batch that stopped part-way is retried with its own key)",
			len(body.Items), maxRequestHandoffItems), http.StatusBadRequest)
		return body, false
	}
	return body, true
}

// handoffDependsOn reads one item's dependencies as the strings CreateTasks parses:
// refs as written, ticket ids as "123" (its `#123` form is also accepted there). A
// JSON number is an id; anything else in the list is refused, naming the item and the
// position, because a silently dropped dependency is a ticket that starts before the
// work it needs.
func handoffDependsOn(raw []json.RawMessage, i int) ([]string, error) {
	out := make([]string, 0, len(raw))
	for j, entry := range raw {
		var s string
		if err := json.Unmarshal(entry, &s); err == nil {
			out = append(out, s)
			continue
		}
		var n json.Number
		if err := json.Unmarshal(entry, &n); err == nil {
			id, convErr := strconv.Atoi(n.String())
			if convErr != nil || id <= 0 {
				return nil, authz.RequestInvalid("items[%d].depends_on[%d] must be a ref or a whole ticket id (got %s)", i, j, n.String())
			}
			out = append(out, strconv.Itoa(id))
			continue
		}
		return nil, authz.RequestInvalid("items[%d].depends_on[%d] must be a ref (string) or a ticket id (number), not %s",
			i, j, strings.TrimSpace(string(entry)))
	}
	return out, nil
}
