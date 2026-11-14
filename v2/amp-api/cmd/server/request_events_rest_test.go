package main

// The request's history read over REST (AMP #4271, epic 202 story 549).
//
// request_reads_rest_test.go proves the two request reads' view matrix and
// internal/service/request_events_test.go proves the read's arms and its paging.
// This file asks the questions only the ROUTE can answer:
//
//  1. THE ROUTE IS THE READ request_events NEVER HAD. A member of the inbox gets
//     the request's history under /api/teams/{id}/requests/{request}/events,
//     keyed by serial or by the requester's opaque reference, in append order,
//     with every stored field — and the rows are the ones the PRODUCT wrote (the
//     submission's own `submit` row plus the claim, release and assign rows the
//     write routes beside this one appended), not fixtures inserted behind the
//     service's back.
//  2. THE ANSWER IS THE CLIENT'S OWN TYPE. The page decodes into a struct that is
//     field-for-field the UI's existing RequestEvent (types/index.ts:1912) with
//     unknown fields refused, so a shape this route invented would fail here
//     instead of in the history rail. Nothing is added to the client's types by
//     this ticket.
//  3. THE VIEWS AND THE TENANT DO NOT DEPEND ON THE ROUTE. A requester who is not
//     on the inbox is refused (404, the surface's one code — the projection has no
//     field for a history row), and another requester's request, a sibling team's
//     request, another org's request and a team id that was never issued are the
//     same bytes, identical to the detail route's refusal. A submit-only
//     credential is refused before the handler runs; after_id/limit page the
//     history forward, and a malformed one is request.invalid naming itself.
//
// Everything goes through the real mux (api.RestHandler.Register, so deniedMux,
// the submit-scope gate and the denied() audit sit in front of the handler) and
// the real auth verifier, on the scratch database newReadsWorld builds.
//
// Run against a disposable Postgres (never a live stack), per
// how-to/run-one-amp-api-test-with-disposable-postgres.md:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestRESTRequestEvents' -count=1 -race -v

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// uiRequestEvent is the UI's RequestEvent (v2/ui/src/types/index.ts), field for
// field and tag for tag. It is deliberately a copy: decoding the route's answer
// into it with unknown fields refused is what proves "the answer decodes into the
// client's existing type with no client shape change". If the server ever adds a
// field to the row, this test fails — which is the point.
type uiRequestEvent struct {
	ID          int64                     `json:"id"`
	RequestID   int                       `json:"request_id"`
	OrgID       int                       `json:"org_id"`
	TeamID      int                       `json:"team_id"`
	Action      domain.RequestEventAction `json:"action"`
	FromStatus  domain.RequestStatus      `json:"from_status"`
	ToStatus    domain.RequestStatus      `json:"to_status"`
	Actor       string                    `json:"actor"`
	ActorUserID *int                      `json:"actor_user_id"`
	Detail      string                    `json:"detail"`
	CreatedAt   time.Time                 `json:"created_at"`
}

// eventsPath names the history read for one request. request is a serial or a
// public reference, exactly as the detail path accepts either.
func (w *readsWorld) eventsPath(teamID int, request, query string) string {
	p := fmt.Sprintf("/api/teams/%d/requests/%s/events", teamID, request)
	if query != "" {
		p += "?" + query
	}
	return p
}

// historyAnswer is one history page, decoded into the client's own type.
type historyAnswer struct {
	View        string           `json:"view"`
	RequestID   int              `json:"request_id"`
	Count       int              `json:"count"`
	NextAfterID int64            `json:"next_after_id"`
	Events      []uiRequestEvent `json:"events"`
}

// decodeHistory reads a history response as the client would: unknown fields are
// refused, so a shape the UI does not have cannot pass as "decoded fine".
func (w *readsWorld) decodeHistory(r readsResult) historyAnswer {
	w.t.Helper()
	if r.status != 200 {
		w.t.Fatalf("history = %d %v, want 200", r.status, r.body)
	}
	var out historyAnswer
	dec := json.NewDecoder(strings.NewReader(r.raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		w.t.Fatalf("the history answer does not decode into the client's type: %v\n%s", err, r.raw)
	}
	return out
}

// TestRESTRequestEvents_TeamHistory is criteria 1 and 2 for an inbox member: the
// rows are the acts the product performed, in append order, in the client's shape.
func TestRESTRequestEvents_TeamHistory(t *testing.T) {
	w := newReadsWorld(t)
	target := w.mined[0] // the requester's, assigned to the member

	// Three acts through the real write routes beside this read. The submission
	// itself already wrote a `submit` row, so the history starts with one.
	// The transfer names the org owner, not the member who already holds the
	// request: assigning the row the assignee it already has is a replay that
	// writes nothing (and so appends no row), which is a property of the write
	// path and not of this read.
	w.mustPost(w.memberTok, w.writePath(w.team, strconv.Itoa(target.ID), "claim"), nil)
	w.mustPost(w.memberTok, w.writePath(w.team, strconv.Itoa(target.ID), "release"), nil)
	w.mustPost(w.adminTokOrFail(t), w.writePath(w.team, strconv.Itoa(target.ID), "assign"),
		map[string]any{"assignee": strconv.Itoa(w.admin.User.ID)})

	// What the table holds, read directly: the route must answer exactly this
	// sequence, because a route that re-sorted or filtered would still "pass" a
	// hand-written expectation.
	want := w.historyActions(target.ID)
	if len(want) != 4 {
		t.Fatalf("the table holds %d rows for the request, want the submit plus three acts: %v", len(want), want)
	}

	for _, tc := range []struct{ name, request string }{
		{"by serial", strconv.Itoa(target.ID)},
		{"by reference", "REQ-" + target.PublicRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := w.decodeHistory(w.do(w.memberTok, w.eventsPath(w.team, tc.request, "")))
			if answer.View != "team" {
				t.Fatalf("view = %q, want \"team\" for an inbox member", answer.View)
			}
			if answer.RequestID != target.ID {
				t.Fatalf("request_id = %d, want %d (the rail pins a stale read with it)", answer.RequestID, target.ID)
			}
			if answer.Count != len(want) || len(answer.Events) != len(want) {
				t.Fatalf("count %d, %d rows; want the %d rows the product wrote", answer.Count, len(answer.Events), len(want))
			}
			if answer.NextAfterID != 0 {
				t.Fatalf("next_after_id = %d on a page that holds the whole history", answer.NextAfterID)
			}
			// Append order is id order, and the actions are the ones performed.
			for i, ev := range answer.Events {
				if ev.Action != want[i] {
					t.Fatalf("row %d action = %q, want %q (append order): %+v", i, ev.Action, want[i], answer.Events)
				}
				if i > 0 && ev.ID <= answer.Events[i-1].ID {
					t.Fatalf("rows are not in increasing id order: %d then %d", answer.Events[i-1].ID, ev.ID)
				}
			}
			// The stored row's own fields: the tenant, the actor and the time are
			// all there, and the claim row's actor is the member who made it.
			first := answer.Events[0]
			if first.RequestID != target.ID || first.OrgID != w.org || first.TeamID != w.team {
				t.Fatalf("row 0 lost its identity: %+v", first)
			}
			if first.Action != domain.RequestActionSubmit {
				t.Fatalf("row 0 action = %q, want the submission's submit row", first.Action)
			}
			if first.Actor == "" || first.CreatedAt.IsZero() {
				t.Fatalf("row 0 lost its attribution or its time: %+v", first)
			}
			claim := answer.Events[1]
			if claim.Action != domain.RequestActionClaim {
				t.Fatalf("row 1 action = %q, want claim", claim.Action)
			}
			if claim.ActorUserID == nil || *claim.ActorUserID != w.member.User.ID {
				t.Fatalf("the claim's actor_user_id = %v, want the member %d", claim.ActorUserID, w.member.User.ID)
			}
		})
	}

	// A request with no acts of its own yet is an empty list, not null and not a
	// 404: "nothing has happened" is an answer the rail must be able to render.
	// The fixture's requests all carry a submit row, so this needs its own.
	quiet := w.freshEventlessRequest()
	silent := w.decodeHistory(w.do(w.memberTok, w.eventsPath(w.team, strconv.Itoa(quiet), "")))
	if silent.Count != 0 || len(silent.Events) != 0 {
		t.Fatalf("a request with no acts = %+v, want an empty list", silent)
	}
	if strings.Contains(w.do(w.memberTok, w.eventsPath(w.team, strconv.Itoa(quiet), "")).raw, `"events":null`) {
		t.Fatal("an empty history encodes as null: a client cannot tell it from \"not served\"")
	}
}

// TestRESTRequestEvents_RequesterIsRefused is criterion 1's requester half: the
// projection has no field for a history row, so the route answers the surface's
// one 404 — the same bytes the detail route gives for something the caller may
// not see.
func TestRESTRequestEvents_RequesterIsRefused(t *testing.T) {
	w := newReadsWorld(t)
	target := w.mined[0] // the requester's own request
	w.mustPost(w.memberTok, w.writePath(w.team, strconv.Itoa(target.ID), "claim"), nil)

	// The detail route answers this requester 200 for the same request: they may
	// read it, projected.
	if detail := w.do(w.requesterTok, w.detailPath(w.team, strconv.Itoa(target.ID))); detail.status != 200 {
		t.Fatalf("requester detail = %d %v, want 200", detail.status, detail.body)
	}
	// The history route refuses it, and nothing internal is in the bytes.
	refused := w.do(w.requesterTok, w.eventsPath(w.team, strconv.Itoa(target.ID), ""))
	w.wantCode(refused, 404, authz.CodeRequestNotFound)
	for _, leak := range []string{"claim", "actor", "detail", "events"} {
		if strings.Contains(refused.raw, leak) {
			t.Fatalf("the refusal carries %q: %s", leak, refused.raw)
		}
	}
	// The refusal is the detail route's own answer for a request this person may
	// not see at all, so a requester cannot use the history route to learn
	// something the detail route hides.
	hidden := w.do(w.requesterTok, w.detailPath(w.team, strconv.Itoa(w.sibling.ID)))
	if hidden.status != 404 || hidden.raw != refused.raw {
		t.Fatalf("the detail route answers %s (%d), the history route %s: they must be one refusal",
			hidden.raw, hidden.status, refused.raw)
	}
	// A requester reading their own history by reference is refused identically:
	// the refusal is not the serial's.
	byRef := w.do(w.requesterTok, w.eventsPath(w.team, "REQ-"+target.PublicRef, ""))
	if byRef.status != 404 || byRef.raw != refused.raw {
		t.Fatalf("the reference route answers %s (%d), want the same refusal %s", byRef.raw, byRef.status, refused.raw)
	}
}

// TestRESTRequestEvents_TenantAndViewMatrix is criterion 1's 404 half and the
// scope gate: every way of not being allowed to read a request's history is one
// answer, and it does not depend on this route existing.
func TestRESTRequestEvents_TenantAndViewMatrix(t *testing.T) {
	w := newReadsWorld(t)
	target := w.mined[0]
	w.mustPost(w.memberTok, w.writePath(w.team, strconv.Itoa(target.ID), "claim"), nil)

	// Another team's request, read through this inbox's own path: 404, not 403 —
	// a 403 would confirm the request exists somewhere.
	sibling := w.do(w.memberTok, w.eventsPath(w.team, strconv.Itoa(w.sibling.ID), ""))
	w.wantCode(sibling, 404, authz.CodeRequestNotFound)

	// Another org's request, and the foreign desk named directly by its own
	// member: the tenant is the request's, not a guess.
	foreign := w.do(w.memberTok, w.eventsPath(w.team, strconv.Itoa(w.foreignReq.ID), ""))
	w.wantCode(foreign, 404, authz.CodeRequestNotFound)
	if ownDesk := w.do(w.strangerTok, w.eventsPath(w.foreign, strconv.Itoa(w.foreignReq.ID), "")); ownDesk.status != 200 {
		t.Fatalf("the foreign desk's own member = %d %v, want 200 (the tenant is the request's, not a guess)",
			ownDesk.status, ownDesk.body)
	}

	// Another requester's request, an id that never existed and an unissued team
	// are the same bytes as the sibling's refusal.
	anothers := w.do(w.otherTok, w.eventsPath(w.team, strconv.Itoa(target.ID), ""))
	w.wantCode(anothers, 404, authz.CodeRequestNotFound)
	unknown := w.do(w.memberTok, w.eventsPath(w.team, "999999", ""))
	w.wantCode(unknown, 404, authz.CodeRequestNotFound)
	unissued := w.do(w.memberTok, w.eventsPath(999999, strconv.Itoa(target.ID), ""))
	w.wantCode(unissued, 404, authz.CodeRequestNotFound)
	if sibling.raw != unknown.raw || anothers.raw != unknown.raw || unissued.raw != unknown.raw || foreign.raw != unknown.raw {
		t.Fatalf("the refusals differ:\n sibling=%s\n anothers=%s\n unissued=%s\n foreign=%s\n unknown=%s",
			sibling.raw, anothers.raw, unissued.raw, foreign.raw, unknown.raw)
	}
	// And the same refusal the detail route gives for the same request.
	if detail := w.do(w.memberTok, w.detailPath(w.team, strconv.Itoa(w.sibling.ID))); detail.raw != sibling.raw {
		t.Fatalf("the detail route answers %s, the history route %s", detail.raw, sibling.raw)
	}

	// A submit-only credential reaches the POST it was minted for and nothing
	// else — this read included, refused by the gate rather than by a handler.
	scoped := w.do(w.scopedTok, w.eventsPath(w.team, strconv.Itoa(target.ID), ""))
	if scoped.status != 403 {
		t.Fatalf("submit-only history read = %d %v, want 403", scoped.status, scoped.body)
	}
	if code, _ := scoped.body["code"].(string); code != authz.CodeTokenScopeDenied {
		t.Fatalf("submit-only code = %v, want %q", scoped.body["code"], authz.CodeTokenScopeDenied)
	}

	// Signed out is 401: the read never ran.
	if anon := w.do("", w.eventsPath(w.team, strconv.Itoa(target.ID), "")); anon.status != 401 {
		t.Fatalf("no token = %d, want 401", anon.status)
	}
}

// TestRESTRequestEvents_PagingAndRefusals is criterion 1's paging half and the
// parameter contract: after_id/limit page the history forward exactly once, and a
// malformed one is request.invalid naming itself.
func TestRESTRequestEvents_PagingAndRefusals(t *testing.T) {
	w := newReadsWorld(t)
	target := w.freshEventlessRequest()
	const acts = 5
	// Five acts through the real claim/release routes, so the rows are the
	// product's and the ids are the database's.
	for i := 0; i < acts; i++ {
		act := "claim"
		if i%2 == 1 {
			act = "release"
		}
		w.mustPost(w.memberTok, w.writePath(w.team, strconv.Itoa(target), act), nil)
	}
	all := w.decodeHistory(w.do(w.memberTok, w.eventsPath(w.team, strconv.Itoa(target), "limit=10000")))
	if all.Count != acts || all.NextAfterID != 0 {
		t.Fatalf("a clamped limit = %d rows, cursor %d; want %d and none", all.Count, all.NextAfterID, acts)
	}

	var seen []int64
	cursor := int64(0)
	for page := 0; page < 10; page++ {
		q := "limit=2"
		if cursor != 0 {
			q += "&after_id=" + strconv.FormatInt(cursor, 10)
		}
		answer := w.decodeHistory(w.do(w.memberTok, w.eventsPath(w.team, strconv.Itoa(target), q)))
		if answer.Count != len(answer.Events) {
			t.Fatalf("page %d: count %d != %d rows", page, answer.Count, len(answer.Events))
		}
		if len(answer.Events) > 2 {
			t.Fatalf("page %d holds %d rows for limit=2", page, len(answer.Events))
		}
		for _, ev := range answer.Events {
			seen = append(seen, ev.ID)
		}
		if answer.NextAfterID == 0 {
			break
		}
		cursor = answer.NextAfterID
	}
	if len(seen) != acts {
		t.Fatalf("paging yielded %d rows, want %d: %v", len(seen), acts, seen)
	}
	for i, id := range seen {
		if id != all.Events[i].ID {
			t.Fatalf("paged row %d is %d, want %d: paging is not stable", i, id, all.Events[i].ID)
		}
	}

	// A page that lands on the last row has no cursor; one row short, it does.
	lastOne := w.decodeHistory(w.do(w.memberTok,
		w.eventsPath(w.team, strconv.Itoa(target), "after_id="+strconv.FormatInt(seen[len(seen)-2], 10))))
	if lastOne.Count != 1 || lastOne.NextAfterID != 0 {
		t.Fatalf("the last page = %d rows, cursor %d; want 1 and none", lastOne.Count, lastOne.NextAfterID)
	}
	short := w.decodeHistory(w.do(w.memberTok,
		w.eventsPath(w.team, strconv.Itoa(target), "after_id="+strconv.FormatInt(seen[len(seen)-3], 10)+"&limit=1")))
	if short.Count != 1 || short.NextAfterID != seen[len(seen)-2] {
		t.Fatalf("a one-row page = %+v, want the row %d and its cursor", short, seen[len(seen)-2])
	}

	// Malformed parameters are the caller's mistake: 400 request.invalid, naming
	// the parameter, never a silently ignored filter.
	for _, tc := range []struct{ name, query string }{
		{"non-numeric after_id", "after_id=abc"},
		{"negative after_id", "after_id=-1"},
		{"non-numeric limit", "limit=lots"},
		{"negative limit", "limit=-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := w.do(w.memberTok, w.eventsPath(w.team, strconv.Itoa(target), tc.query))
			w.wantCode(res, 400, authz.CodeRequestInvalid)
			if msg, _ := res.body["error"].(string); strings.TrimSpace(msg) == "" {
				t.Fatalf("%s refusal has no message", tc.name)
			}
		})
	}

	// A value that could never be a reference is 400, not a 404: the decoder is
	// the detail read's, so the two routes refuse it identically.
	bogus := w.do(w.memberTok, w.eventsPath(w.team, "not-a-reference!", ""))
	w.wantCode(bogus, 400, authz.CodeRequestInvalid)

	// A read is looking, not an act: no audit row for it.
	before := w.auditRows(w.member.User.ID, "request.read")
	w.do(w.memberTok, w.eventsPath(w.team, strconv.Itoa(target), ""))
	if got := w.auditRows(w.member.User.ID, "request.read"); got != before {
		t.Fatalf("the history read wrote %d request.read audit rows", got-before)
	}
}

// ---- helpers ----

// mustPost issues a write and fails the test unless it succeeded.
func (w *readsWorld) mustPost(token, path string, body any) {
	w.t.Helper()
	res := w.send(token, http.MethodPost, path, body, nil)
	if res.status != 200 && res.status != http.StatusCreated {
		w.t.Fatalf("POST %s = %d %s", path, res.status, res.raw)
	}
}

// historyActions reads a request's history straight from the table: the actions
// in append order. It is the expectation the route is compared against, and
// reading the table rather than re-deriving it from the acts keeps the test from
// asserting its own arithmetic.
func (w *readsWorld) historyActions(requestID int) []domain.RequestEventAction {
	w.t.Helper()
	rows, err := w.db.Query(w.ctx, `SELECT action FROM request_events WHERE request_id = $1 ORDER BY id`, requestID)
	if err != nil {
		w.t.Fatalf("history of request %d: %v", requestID, err)
	}
	defer rows.Close()
	var out []domain.RequestEventAction
	for rows.Next() {
		var a domain.RequestEventAction
		if err := rows.Scan(&a); err != nil {
			w.t.Fatalf("scan action: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		w.t.Fatalf("history of request %d: %v", requestID, err)
	}
	return out
}

// freshEventlessRequest returns a request in the world's inbox that has no
// history rows yet, deleting the submit row the submission path wrote (the read
// under test is the only thing that could notice, and "a request with no acts"
// is a state 053's schema allows — a service- or connector-submitted request
// whose act landed in an earlier transaction, for instance). It is a fixture
// concern, not a product one: nothing in the product deletes a history row.
func (w *readsWorld) freshEventlessRequest() int {
	w.t.Helper()
	req := w.freshRequest(w.requester, "eventless")
	if _, err := w.db.Exec(w.ctx, `DELETE FROM request_events WHERE request_id = $1`, req.ID); err != nil {
		w.t.Fatalf("clear the history of request %d: %v", req.ID, err)
	}
	return req.ID
}
