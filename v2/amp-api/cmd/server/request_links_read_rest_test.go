package main

// The request's linked-work read over REST (AMP #4272, epic 202 story 549).
//
// internal/service/request_links_read_test.go proves the service's arms (who may
// read, the requester's refusal, the kind filter) and
// internal/repository/request_links_test.go proves the read's tenant scope and
// ordering. This file asks the questions only the ROUTE can answer:
//
//  1. THE ROUTE IS THE READ `POST …/links` NEVER HAD. A member of the inbox gets
//     the request's lineage under /api/teams/{id}/requests/{request}/links, keyed
//     by serial or by the requester's opaque reference, in attach order, filterable
//     by kind — and the rows are the ones the PRODUCT wrote (the attach route
//     beside this read, through the real service), not fixtures inserted behind
//     its back.
//  2. THE ANSWER IS THE CLIENT'S OWN TYPE. The answer decodes into a struct that is
//     field-for-field the UI's existing RequestLink (types/index.ts:1940) with
//     unknown fields refused, and its rows are field-for-field the UI's
//     ResolvedRequestLink minus its optional, client-resolved `target` — so a shape
//     this route invented would fail here instead of in the planning rail. Nothing
//     is added to the client's types by this ticket.
//  3. THE VIEWS AND THE TENANT DO NOT DEPEND ON THE ROUTE. A requester who is not
//     on the inbox is refused (404, the surface's one code — the projection
//     carries the summary in `milestones` and has no field for a link row), and
//     another team's request, another org's request and a team id that was never
//     issued are the same bytes, identical to the detail route's refusal. A
//     submit-only credential is refused before the handler runs; an unknown or
//     malformed `kind` is request.invalid naming itself.
//
// Everything goes through the real mux (api.RestHandler.Register, so deniedMux,
// the submit-scope gate and the denied() audit sit in front of the handler) and
// the real auth verifier, on the scratch database newLinksWorld builds — the same
// world the attach route's suite uses, so a link made by the product is what this
// read reads back.
//
// Run against a disposable Postgres (never a live stack), per
// how-to/run-one-amp-api-test-with-disposable-postgres.md:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestRESTRequestLinksRead' -count=1 -race -v

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// uiRequestLink is the UI's RequestLink (v2/ui/src/types/index.ts:1940), field
// for field and tag for tag. It is deliberately a copy: decoding the route's
// answer into it with unknown fields refused is what proves "the answer decodes
// into the client's existing type with no client shape change". If the server
// ever adds a field to the row, this test fails — which is the point.
//
// ResolvedRequestLink (v2/ui/src/lib/requestControls.ts:229) is `interface
// ResolvedRequestLink extends RequestLink { target?: RequestLinkTarget }`: the
// extension is OPTIONAL and resolved by the client from the target's own read
// (its title, project, approved/merged facts and `gone` tombstone), so this route
// answers the base type and the client's type still decodes it. `uiResolvedLink`
// below states that pairing.
type uiRequestLink struct {
	ID         int64                    `json:"id"`
	RequestID  int                      `json:"request_id"`
	LinkKind   domain.RequestLinkKind   `json:"link_kind"`
	TargetKind domain.RequestTargetKind `json:"target_kind"`
	TargetID   int                      `json:"target_id"`
	CreatedBy  *int                     `json:"created_by"`
	CreatedAt  time.Time                `json:"created_at"`
}

// uiResolvedLink is the UI's ResolvedRequestLink: the row plus the optional
// resolved target. Decoding this route's rows into it proves the extension is the
// optional half the client fills, not a field the server owes.
type uiResolvedLink struct {
	uiRequestLink
	Target *struct {
		Kind      domain.RequestTargetKind `json:"kind"`
		ID        int                      `json:"id"`
		Title     string                   `json:"title"`
		ProjectID int                      `json:"projectId"`
	} `json:"target"`
}

// linksReadPath names the read for one request; request is a serial or a public
// reference, exactly as the detail path accepts either.
func (w *linksWorld) linksReadPath(teamID int, request, query string) string {
	p := fmt.Sprintf("/api/teams/%d/requests/%s/links", teamID, request)
	if query != "" {
		p += "?" + query
	}
	return p
}

// linksAnswer is one lineage page, decoded into the client's own type.
type linksAnswer struct {
	View      string          `json:"view"`
	RequestID int             `json:"request_id"`
	Count     int             `json:"count"`
	Links     []uiRequestLink `json:"links"`
	RawLinks  json.RawMessage `json:"-"`
}

// decodeLinks reads a links response as the client would: unknown fields are
// refused, so a shape the UI does not have cannot pass as "decoded fine".
func (w *linksWorld) decodeLinks(r writeResult) linksAnswer {
	w.t.Helper()
	if r.status != 200 {
		w.t.Fatalf("links = %d %v, want 200", r.status, r.body)
	}
	var out linksAnswer
	dec := json.NewDecoder(strings.NewReader(r.raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		w.t.Fatalf("the links answer does not decode into the client's type: %v\n%s", err, r.raw)
	}
	return out
}

// attachThroughRoute writes one relation through the real attach route, so every
// row this read answers with was made by the product.
func (w *linksWorld) attachThroughRoute(requestID int, kind, target string, targetID int) {
	w.t.Helper()
	res := w.send(w.memberTok, http.MethodPost, w.linkPath(w.team, requestID), map[string]any{
		"link_kind": kind, "target_kind": target, "target_id": targetID,
	}, nil)
	w.wantWriteCode(res, 201, "")
}

// submitIn writes one request into teamID through the real submission path, so a
// request in another team or another org is a row the product made rather than a
// fixture's insert. newLinksWorld submits only into its own inbox; these are the
// two tenants the refusals are about.
func (w *linksWorld) submitIn(who *auth.Identity, teamID int, subject string) *domain.TeamRequest {
	w.t.Helper()
	res, err := w.svc.SubmitRequest(w.ctx, w.principalOf(who.User.ID), service.SubmitRequestInput{
		TeamID: teamID, Source: domain.RequestSourceWeb,
		Subject: subject, InputBrief: "The details are in the request body.",
	})
	if err != nil {
		w.t.Fatalf("submit %q into team %d: %v", subject, teamID, err)
	}
	return res.Request
}

// TestRESTRequestLinksRead_TeamLineage is criteria 1 and 2 for an inbox member:
// the rows are the relations the attach route wrote, in attach order, in the
// client's shape.
func TestRESTRequestLinksRead_TeamLineage(t *testing.T) {
	w := newLinksWorld(t)
	req := w.mined[1] // the requester's, claimed by the member: a request triage is working

	// One of each relation the two rails draw, through the real write route. The
	// route is attached first because a `routes_to` re-route is D9's separate act.
	w.attachThroughRoute(req.ID, "routes_to", "project", w.project)
	w.attachThroughRoute(req.ID, "planned_as", "epic", w.epic)
	w.attachThroughRoute(req.ID, "planned_as", "story", w.story)
	w.attachThroughRoute(req.ID, "planned_as", "task", w.deliverable)

	// What the table holds, read directly: the route must answer exactly this
	// sequence, because a route that re-sorted or filtered would still "pass" a
	// hand-written expectation.
	want := w.linkRows(req.ID)
	if len(want) != 4 {
		t.Fatalf("the table holds %d rows for the request, want the four attached: %+v", len(want), want)
	}

	for _, tc := range []struct{ name, request string }{
		{"by serial", strconv.Itoa(req.ID)},
		{"by reference", "REQ-" + req.PublicRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := w.decodeLinks(w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, tc.request, ""), nil, nil))
			if answer.View != "team" {
				t.Fatalf("view = %q, want \"team\" for an inbox member", answer.View)
			}
			if answer.RequestID != req.ID {
				t.Fatalf("request_id = %d, want %d (a rail pins a stale read with it)", answer.RequestID, req.ID)
			}
			if answer.Count != len(want) || len(answer.Links) != len(want) {
				t.Fatalf("count %d, %d rows; want the %d the product wrote", answer.Count, len(answer.Links), len(want))
			}
			// Attach order is id order, and the relations are the ones attached.
			for i, l := range answer.Links {
				if l.ID != want[i].ID {
					t.Fatalf("row %d is link %d, want %d (attach order): %+v", i, l.ID, want[i].ID, answer.Links)
				}
				if l.LinkKind != want[i].LinkKind || l.TargetKind != want[i].TargetKind || l.TargetID != want[i].TargetID {
					t.Fatalf("row %d = %s→%s %d, want %s→%s %d",
						i, l.LinkKind, l.TargetKind, l.TargetID,
						want[i].LinkKind, want[i].TargetKind, want[i].TargetID)
				}
			}
			// The stored row's own fields: the request it belongs to, the triager who
			// attached it and when — the three a rail renders attribution from.
			for _, l := range answer.Links {
				if l.RequestID != req.ID {
					t.Fatalf("a row names request %d, want %d", l.RequestID, req.ID)
				}
				if l.CreatedBy == nil || *l.CreatedBy != w.member.User.ID {
					t.Fatalf("a row's created_by = %v, want the member %d", l.CreatedBy, w.member.User.ID)
				}
				if l.CreatedAt.IsZero() {
					t.Fatalf("a row lost its created_at: %+v", l)
				}
			}
		})
	}

	// A request with nothing attached is an empty list, not null and not a 404:
	// "nothing is attached yet" is an answer the planning rail must be able to
	// render ("Not routed yet"), and null reads as "not served".
	quiet := w.mined[3]
	silent := w.decodeLinks(w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(quiet.ID), ""), nil, nil))
	if silent.Count != 0 || len(silent.Links) != 0 {
		t.Fatalf("an unattached request = %+v, want an empty list", silent)
	}
	if strings.Contains(w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(quiet.ID), ""), nil, nil).raw, `"links":null`) {
		t.Fatal("an empty lineage encodes as null: a client cannot tell it from \"not served\"")
	}

	// And the answer decodes into the UI's ResolvedRequestLink too (criterion 2):
	// its `target` is the OPTIONAL, client-resolved half, so the base rows must
	// decode with it absent rather than the server inventing a title.
	res := w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(req.ID), ""), nil, nil)
	var resolved struct {
		View      string           `json:"view"`
		RequestID int              `json:"request_id"`
		Count     int              `json:"count"`
		Links     []uiResolvedLink `json:"links"`
	}
	dec := json.NewDecoder(strings.NewReader(res.raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resolved); err != nil {
		t.Fatalf("the rows do not decode into the UI's ResolvedRequestLink: %v\n%s", err, res.raw)
	}
	if len(resolved.Links) != len(want) {
		t.Fatalf("ResolvedRequestLink decode = %d rows, want %d", len(resolved.Links), len(want))
	}
	for _, l := range resolved.Links {
		if l.Target != nil {
			t.Fatalf("the server invented a resolved target %+v: resolving it is the client's read of the target's own row", l.Target)
		}
		if l.ID == 0 || l.LinkKind == "" || l.TargetKind == "" || l.TargetID == 0 {
			t.Fatalf("a row lost a field ResolvedRequestLink inherits from RequestLink: %+v", l)
		}
	}
}

// TestRESTRequestLinksRead_FilterAndRefusals is criterion 1's filter half and the
// parameter contract: `kind` narrows the answer, in every spelling its siblings'
// filters accept, and anything it cannot act on is request.invalid naming itself
// rather than a silently dropped filter.
func TestRESTRequestLinksRead_FilterAndRefusals(t *testing.T) {
	w := newLinksWorld(t)
	req := w.mined[1]
	w.attachThroughRoute(req.ID, "routes_to", "project", w.project)
	w.attachThroughRoute(req.ID, "planned_as", "epic", w.epic)
	w.attachThroughRoute(req.ID, "planned_as", "task", w.deliverable)

	planned := w.decodeLinks(w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(req.ID), "kind=planned_as"), nil, nil))
	if planned.Count != 2 || len(planned.Links) != 2 {
		t.Fatalf("kind=planned_as = %d rows, want the epic and the ticket: %+v", planned.Count, planned.Links)
	}
	for _, l := range planned.Links {
		if l.LinkKind != domain.RequestLinkPlannedAs {
			t.Fatalf("the filter returned a %s row: %+v", l.LinkKind, l)
		}
	}

	// The queue's own spelling conventions, applied to this filter: a repeated
	// parameter, a comma-joined list, mixed case and padding.
	for _, query := range []string{
		"kind=planned_as&kind=delivered_by",
		"kind=planned_as,delivered_by",
		"kind=%20PLANNED_AS%20",
	} {
		answer := w.decodeLinks(w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(req.ID), query), nil, nil))
		want := 2
		if strings.Contains(strings.ToLower(query), "delivered_by") {
			// No delivered_by row exists in this fixture's request, so the union is the
			// two planned rows: a filter that never matches must not widen the answer.
			want = 2
		}
		if answer.Count != want {
			t.Fatalf("%s = %d rows, want %d", query, answer.Count, want)
		}
	}

	// A kind with no rows is an empty list, not an error.
	none := w.decodeLinks(w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(req.ID), "kind=relates_to"), nil, nil))
	if none.Count != 0 || none.Links == nil {
		t.Fatalf("kind=relates_to = %+v, want an empty non-nil list", none)
	}

	// A value the filter cannot act on is the caller's mistake: 400 request.invalid
	// naming the parameter, never an empty list a reader would take for "nothing is
	// attached".
	for _, tc := range []struct{ name, query string }{
		{"an unknown kind", "kind=planned"},
		{"a target kind", "kind=task"},
		{"an empty value", "kind="},
		{"one bad value among good ones", "kind=planned_as&kind=shipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(req.ID), tc.query), nil, nil)
			w.wantWriteCode(res, 400, authz.CodeRequestInvalid)
			if msg, _ := res.body["error"].(string); strings.TrimSpace(msg) == "" {
				t.Fatalf("%s refusal has no message", tc.name)
			}
		})
	}

	// A value that could never be a reference is 400, not a 404: the decoder is the
	// detail read's, so the routes refuse it identically.
	bogus := w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, "not-a-reference!", ""), nil, nil)
	w.wantWriteCode(bogus, 400, authz.CodeRequestInvalid)

	// A read is looking, not an act: no audit row for it.
	before := w.auditFor("request.read", req.ID)
	w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(req.ID), ""), nil, nil)
	if got := w.auditFor("request.read", req.ID); got != before {
		t.Fatalf("the links read wrote %d request.read audit rows", got-before)
	}
}

// TestRESTRequestLinksRead_RequesterIsRefused is criterion 3's requester half: the
// projection has no field for a link row, so the route answers the surface's one
// 404 — the same bytes the detail route gives for something the caller may not see.
//
// This is also the ticket's documented decision made executable: a requester is
// refused, not filtered, because their summary of the same facts is the
// projection's milestones, which the detail read they CAN read already carries.
func TestRESTRequestLinksRead_RequesterIsRefused(t *testing.T) {
	w := newLinksWorld(t)
	target := w.mined[0] // the requester's own request
	w.attachThroughRoute(target.ID, "routes_to", "project", w.project)

	// The detail route answers this requester 200 for the same request: they may
	// read it, projected — and the projection carries the milestones that are their
	// summary of the lineage.
	detail := w.send(w.requesterTok, http.MethodGet, fmt.Sprintf("/api/teams/%d/requests/%d", w.team, target.ID), nil, nil)
	if detail.status != 200 {
		t.Fatalf("requester detail = %d %v, want 200", detail.status, detail.body)
	}
	if view, _ := detail.body["view"].(string); view != "requester" {
		t.Fatalf("requester detail view = %q, want \"requester\"", view)
	}
	// The links route refuses it, and nothing internal is in the bytes.
	refused := w.send(w.requesterTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(target.ID), ""), nil, nil)
	w.wantWriteCode(refused, 404, authz.CodeRequestNotFound)
	for _, leak := range []string{"links", "target_kind", "created_by", "routes_to", "project"} {
		if strings.Contains(refused.raw, leak) {
			t.Fatalf("the refusal carries %q: %s", leak, refused.raw)
		}
	}
	// The refusal is the detail route's own answer for a request this person may
	// not see at all, so a requester cannot use this route to learn something the
	// detail route hides.
	hidden := w.send(w.requesterTok, http.MethodGet, fmt.Sprintf("/api/teams/%d/requests/%d", w.team, w.mined[2].ID), nil, nil)
	if hidden.status != 404 || hidden.raw != refused.raw {
		t.Fatalf("the detail route answers %s (%d), the links route %s: they must be one refusal",
			hidden.raw, hidden.status, refused.raw)
	}
	// A requester reading their own lineage by reference is refused identically:
	// the refusal is not the serial's.
	byRef := w.send(w.requesterTok, http.MethodGet, w.linksReadPath(w.team, "REQ-"+target.PublicRef, ""), nil, nil)
	if byRef.status != 404 || byRef.raw != refused.raw {
		t.Fatalf("the reference route answers %s (%d), want the same refusal %s", byRef.raw, byRef.status, refused.raw)
	}
}

// TestRESTRequestLinksRead_TenantAndViewMatrix is criterion 1's 404 half and the
// scope gate: every way of not being allowed to read a request's lineage is one
// answer, and it does not depend on this route existing.
func TestRESTRequestLinksRead_TenantAndViewMatrix(t *testing.T) {
	w := newLinksWorld(t)
	target := w.mined[1]
	w.attachThroughRoute(target.ID, "routes_to", "project", w.project)

	// The two other tenants: a sibling desk in the same org, and a desk in another
	// org. Both are rows the real submission path wrote.
	siblingReq := w.submitIn(w.admin, w.otherTeam, "A sibling desk's request")
	foreignReq := w.submitIn(w.stranger, w.foreign, "Another org's request")

	// Another team's request, read through this inbox's own path: 404, not 403 —
	// a 403 would confirm the request exists somewhere.
	sibling := w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(siblingReq.ID), ""), nil, nil)
	w.wantWriteCode(sibling, 404, authz.CodeRequestNotFound)

	// Another org's request, and the foreign desk named directly by its own
	// member: the tenant is the request's, not a guess.
	foreign := w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(foreignReq.ID), ""), nil, nil)
	w.wantWriteCode(foreign, 404, authz.CodeRequestNotFound)
	if ownDesk := w.send(w.strangerTok, http.MethodGet, w.linksReadPath(w.foreign, strconv.Itoa(foreignReq.ID), ""), nil, nil); ownDesk.status != 200 {
		t.Fatalf("the foreign desk's own member = %d %v, want 200 (the tenant is the request's, not a guess)",
			ownDesk.status, ownDesk.body)
	}

	// Another requester's request, an id that never existed and an unissued team
	// are the same bytes as the sibling's refusal.
	anothers := w.send(w.otherTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(target.ID), ""), nil, nil)
	w.wantWriteCode(anothers, 404, authz.CodeRequestNotFound)
	unknown := w.send(w.memberTok, http.MethodGet, w.linksReadPath(w.team, "999999", ""), nil, nil)
	w.wantWriteCode(unknown, 404, authz.CodeRequestNotFound)
	unissued := w.send(w.memberTok, http.MethodGet, w.linksReadPath(999999, strconv.Itoa(target.ID), ""), nil, nil)
	w.wantWriteCode(unissued, 404, authz.CodeRequestNotFound)
	if sibling.raw != unknown.raw || anothers.raw != unknown.raw || unissued.raw != unknown.raw || foreign.raw != unknown.raw {
		t.Fatalf("the refusals differ:\n sibling=%s\n anothers=%s\n unissued=%s\n foreign=%s\n unknown=%s",
			sibling.raw, anothers.raw, unissued.raw, foreign.raw, unknown.raw)
	}
	// And the same refusal the detail route gives for the same request.
	if detail := w.send(w.memberTok, http.MethodGet, fmt.Sprintf("/api/teams/%d/requests/%d", w.team, siblingReq.ID), nil, nil); detail.raw != sibling.raw {
		t.Fatalf("the detail route answers %s, the links route %s", detail.raw, sibling.raw)
	}

	// A submit-only credential reaches the POST it was minted for and nothing
	// else — this read included, refused by the gate rather than by a handler.
	scoped := w.send(w.scopedTok, http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(target.ID), ""), nil, nil)
	if scoped.status != 403 {
		t.Fatalf("submit-only links read = %d %v, want 403", scoped.status, scoped.body)
	}
	if code, _ := scoped.body["code"].(string); code != authz.CodeTokenScopeDenied {
		t.Fatalf("submit-only code = %v, want %q", scoped.body["code"], authz.CodeTokenScopeDenied)
	}

	// Signed out is 401: the read never ran.
	if anon := w.send("", http.MethodGet, w.linksReadPath(w.team, strconv.Itoa(target.ID), ""), nil, nil); anon.status != 401 {
		t.Fatalf("no token = %d, want 401", anon.status)
	}
}
