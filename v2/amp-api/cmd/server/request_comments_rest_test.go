package main

// The request's discussion read over REST (AMP #4270, epic 202 story 549).
//
// request_reads_rest_test.go proves the two request reads' view matrix;
// internal/service/request_comments_test.go proves the thread's two arms and the
// sharing rule. This file asks the questions only the ROUTE can answer:
//
//  1. THE ROUTE IS THE READ THE POST LACKED, ON THE SAME PATH SHAPE. A member of
//     the inbox gets the request's whole thread under
//     /api/teams/{id}/requests/{request}/comments — the exact path the POST
//     beside it writes — keyed by serial or by the requester's opaque reference;
//     the same request's rows come back in append order with every stored field.
//  2. THE ANSWER IS THE CLIENT'S OWN TYPE. Each row decodes into a struct that
//     is field-for-field the UI's existing RequestComment (types/index.ts:1900)
//     with unknown fields refused, so a shape this route invented would fail
//     here instead of in the page; a requester's rows decode into the UI's
//     RequesterComment the same way. Nothing is added to the client's types by
//     this ticket.
//  3. THE VIEWS AND THE TENANT DO NOT DEPEND ON THE ROUTE. A requester who is
//     not on the inbox reads only the shared kinds of their own request's
//     thread; another requester's request, a sibling team's request, another
//     org's request and a team id that was never issued are the surface's one
//     404 with its stable code, byte for byte; a submit-only credential is
//     refused before the handler runs; after_id/limit page the thread forward,
//     and a malformed one is request.invalid naming itself.
//
// Everything goes through the real mux (api.RestHandler.Register, so deniedMux,
// the submit-scope gate and the denied() audit sit in front of the handler) and
// the real auth verifier, on the scratch database newReadsWorld builds — so the
// rows a comment read answers with are the ones the POST actually wrote, not
// fixtures this file inserted behind the service's back.
//
// Run against a disposable Postgres (never a live stack), per
// how-to/run-one-amp-api-test-with-disposable-postgres.md:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestRESTRequestComments' -count=1 -race -v

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// uiRequestComment is the UI's RequestComment (v2/ui/src/types/index.ts), field
// for field and tag for tag. It is deliberately a copy: decoding the route's
// answer into it with unknown fields refused is what proves "the answer decodes
// into the client's existing type with no client shape change". If the server
// ever adds a field to the row, this test fails — which is the point.
type uiRequestComment struct {
	ID           int64                     `json:"id"`
	RequestID    int                       `json:"request_id"`
	Kind         domain.RequestCommentKind `json:"kind"`
	Body         string                    `json:"body"`
	Author       string                    `json:"author"`
	AuthorUserID *int                      `json:"author_user_id"`
	CreatedAt    time.Time                 `json:"created_at"`
}

// uiRequesterComment is the UI's RequesterComment (same file): the projection's
// four fields and nothing else.
type uiRequesterComment struct {
	Kind domain.RequestCommentKind `json:"kind"`
	From domain.RequesterAuthor    `json:"from"`
	Body string                    `json:"body"`
	At   time.Time                 `json:"at"`
}

// commentsPath names the thread read for one request. request is a serial or a
// public reference, exactly as the detail path accepts either.
func (w *readsWorld) commentsPath(teamID int, request, query string) string {
	p := fmt.Sprintf("/api/teams/%d/requests/%s/comments", teamID, request)
	if query != "" {
		p += "?" + query
	}
	return p
}

// postComment writes one discussion row through the real POST route and returns
// its stored id. It is the write the read under test must agree with: a test that
// inserted rows behind the service would prove the read can see a table, not that
// it can see what the product wrote.
func (w *readsWorld) postComment(token string, teamID, requestID int, kind, body string) int64 {
	w.t.Helper()
	payload, err := json.Marshal(map[string]string{"kind": kind, "body": body})
	if err != nil {
		w.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/teams/%d/requests/%d/comments", w.srv.URL, teamID, requestID),
		bytes.NewReader(payload))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		w.t.Fatalf("post comment = %d %s", resp.StatusCode, raw)
	}
	var answer struct {
		Comment struct {
			ID int64 `json:"id"`
		} `json:"comment"`
		Wrote bool `json:"wrote"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		w.t.Fatalf("post comment answer: %v (%s)", err, raw)
	}
	if answer.Comment.ID == 0 {
		w.t.Fatalf("post comment answered no id: %s", raw)
	}
	return answer.Comment.ID
}

// threadAnswer decodes a thread response strictly: the envelope's own fields,
// and the two item lists as the client's types.
type threadAnswer struct {
	View        string               `json:"view"`
	RequestID   int                  `json:"request_id"`
	Count       int                  `json:"count"`
	NextAfterID int64                `json:"next_after_id"`
	Comments    []uiRequestComment   `json:"comments"`
	Shared      []uiRequesterComment `json:"shared_comments"`
}

// decodeThread reads a thread response as the client would: unknown fields are
// refused, so a shape the UI does not have cannot pass as "decoded fine".
func (w *readsWorld) decodeThread(r readsResult) threadAnswer {
	w.t.Helper()
	if r.status != 200 {
		w.t.Fatalf("thread = %d %v, want 200", r.status, r.body)
	}
	// request_id, count and next_after_id are the envelope's; the two item
	// arrays are the client's own types. A single strict decoder covers both
	// because the struct above names every field either may carry.
	var out threadAnswer
	dec := json.NewDecoder(strings.NewReader(r.raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		w.t.Fatalf("the thread answer does not decode into the client's types: %v\n%s", err, r.raw)
	}
	return out
}

// TestRESTRequestComments_TeamThread is criteria 1 and 2 for an inbox member.
func TestRESTRequestComments_TeamThread(t *testing.T) {
	w := newReadsWorld(t)
	target := w.mined[0]

	// Three rows through the real POST: a remark, a question the desk asks, and
	// an internal note. All three are the team's to read; the first and last are
	// not the requester's.
	remark := w.postComment(w.memberTok, w.team, target.ID, string(domain.RequestCommentDiscussion), "Triage note: route to platform.")
	question := w.postComment(w.memberTok, w.team, target.ID, string(domain.RequestCommentQuestion), "Which quarter is this for?")
	internal := w.postComment(w.memberTok, w.team, target.ID, string(domain.RequestCommentDiscussion), "Internal: the vendor's export is stale.")

	for _, tc := range []struct{ name, request string }{
		{"by serial", strconv.Itoa(target.ID)},
		{"by reference", "REQ-" + target.PublicRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := w.decodeThread(w.do(w.memberTok, w.commentsPath(w.team, tc.request, "")))
			if answer.View != "team" {
				t.Fatalf("view = %q, want \"team\" for an inbox member", answer.View)
			}
			if answer.RequestID != target.ID {
				t.Fatalf("request_id = %d, want %d (the panel pins a stale read with it)", answer.RequestID, target.ID)
			}
			if answer.Shared != nil {
				t.Fatalf("the team's answer also carries a requester projection: %+v", answer.Shared)
			}
			if answer.Count != 3 || len(answer.Comments) != 3 {
				t.Fatalf("count %d, %d rows; want the 3 rows the POST wrote", answer.Count, len(answer.Comments))
			}
			if answer.NextAfterID != 0 {
				t.Fatalf("next_after_id = %d on a page that holds the whole thread", answer.NextAfterID)
			}
			// Append order, and the stored row's own attribution.
			if answer.Comments[0].ID != remark || answer.Comments[1].ID != question || answer.Comments[2].ID != internal {
				t.Fatalf("rows are not in append order: %+v", answer.Comments)
			}
			for _, c := range answer.Comments {
				if c.RequestID != target.ID {
					t.Fatalf("row %d names request %d", c.ID, c.RequestID)
				}
				if c.Author == "" || c.CreatedAt.IsZero() {
					t.Fatalf("row %d lost its attribution or its time: %+v", c.ID, c)
				}
				if c.AuthorUserID == nil || *c.AuthorUserID != w.member.User.ID {
					t.Fatalf("row %d author_user_id = %v, want the member %d", c.ID, c.AuthorUserID, w.member.User.ID)
				}
			}
			if answer.Comments[1].Kind != domain.RequestCommentQuestion {
				t.Fatalf("row %d kind = %q, want the question", answer.Comments[1].ID, answer.Comments[1].Kind)
			}
		})
	}

	// The internal note is the team's: a route that filtered the thread for
	// everyone would hide the record from the people who triage it.
	all := w.decodeThread(w.do(w.memberTok, w.commentsPath(w.team, strconv.Itoa(target.ID), "")))
	found := false
	for _, c := range all.Comments {
		if c.ID == internal && strings.Contains(c.Body, "vendor's export is stale") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the team's thread is missing its own internal note: %+v", all.Comments)
	}

	// A request with no discussion is an empty list, not null and not a 404:
	// "nothing has been said" is an answer the panel must be able to render.
	empty := w.decodeThread(w.do(w.memberTok, w.commentsPath(w.team, strconv.Itoa(w.mined[1].ID), "")))
	if empty.Count != 0 || len(empty.Comments) != 0 {
		t.Fatalf("a request with no comments = %+v, want an empty list", empty)
	}
	if strings.Contains(w.do(w.memberTok, w.commentsPath(w.team, strconv.Itoa(w.mined[1].ID), "")).raw, `"comments":null`) {
		t.Fatal("an empty thread encodes as null: a client cannot tell it from \"not served\"")
	}
}

// TestRESTRequestComments_RequesterSeesOnlyShared is criterion 1's requester
// half and criterion 2's other type: the requester's rows decode into the UI's
// RequesterComment, which has no field for an id, an author or a user id.
func TestRESTRequestComments_RequesterSeesOnlyShared(t *testing.T) {
	w := newReadsWorld(t)
	target := w.mined[0] // the requester's own request
	w.postComment(w.memberTok, w.team, target.ID, string(domain.RequestCommentDiscussion), "Internal note: do not share.")
	w.postComment(w.memberTok, w.team, target.ID, string(domain.RequestCommentQuestion), "Which quarter is this for?")
	w.postComment(w.requesterTok, w.team, target.ID, string(domain.RequestCommentAnswer), "Q3, for the September release.")
	w.postComment(w.memberTok, w.team, target.ID, string(domain.RequestCommentDiscussion), "Another internal remark.")

	answer := w.decodeThread(w.do(w.requesterTok, w.commentsPath(w.team, strconv.Itoa(target.ID), "")))
	if answer.View != "requester" {
		t.Fatalf("view = %q, want \"requester\"", answer.View)
	}
	if answer.Comments != nil {
		t.Fatalf("a requester's answer carries the team's rows: %+v", answer.Comments)
	}
	if len(answer.Shared) != 2 {
		t.Fatalf("shared_comments = %+v, want the question and the answer only", answer.Shared)
	}
	for _, c := range answer.Shared {
		if strings.Contains(c.Body, "Internal") {
			t.Fatalf("internal text reached the requester: %q", c.Body)
		}
	}
	if answer.Shared[0].Kind != domain.RequestCommentQuestion || answer.Shared[0].From != domain.RequesterAuthorDesk {
		t.Fatalf("the question = %+v, want kind question from the desk", answer.Shared[0])
	}
	if answer.Shared[1].Kind != domain.RequestCommentAnswer || answer.Shared[1].From != domain.RequesterAuthorYou {
		t.Fatalf("the answer = %+v, want kind answer from \"you\"", answer.Shared[1])
	}
	// No cursor: the requester's rows carry no id to continue from.
	if answer.NextAfterID != 0 {
		t.Fatalf("the requester's answer carries next_after_id %d", answer.NextAfterID)
	}

	// Nothing internal in the bytes: the sharing rule is a whitelist, and the
	// facts a requester's surface has no field for — the internal serial, a
	// comment id, an author's display name, an author's user id — are absent
	// rather than merely unreferenced. The envelope drops request_id for this arm
	// for the same reason: a requester addresses their request by its public
	// reference, never by the team's serial.
	raw := w.do(w.requesterTok, w.commentsPath(w.team, strconv.Itoa(target.ID), "")).raw
	for _, leak := range []string{"internal note", "Internal note", `"author"`, `"author_user_id"`, `"request_id"`} {
		if strings.Contains(raw, leak) {
			t.Fatalf("the requester's thread carries %q: %s", leak, raw)
		}
	}
	// And the author's display name, which is a stored value rather than a key.
	var author string
	if err := w.db.QueryRow(w.ctx, `SELECT author FROM request_comments WHERE request_id = $1 AND kind = 'comment' LIMIT 1`, target.ID).
		Scan(&author); err != nil {
		t.Fatalf("author of the internal row: %v", err)
	}
	if author != "" && strings.Contains(raw, author) {
		t.Fatalf("the internal row's author %q reached the requester", author)
	}
}

// TestRESTRequestComments_TenantAndViewMatrix is criterion 1's 404 half and the
// scope gate: every way of not being allowed to read a request is one answer,
// and the refusal does not depend on this route existing.
func TestRESTRequestComments_TenantAndViewMatrix(t *testing.T) {
	w := newReadsWorld(t)
	target := w.mined[0]
	w.postComment(w.memberTok, w.team, target.ID, string(domain.RequestCommentQuestion), "Which quarter?")

	// Another team's request, read through this inbox's own path: 404, not 403 —
	// a 403 would confirm the request exists somewhere.
	sibling := w.do(w.memberTok, w.commentsPath(w.team, strconv.Itoa(w.sibling.ID), ""))
	w.wantCode(sibling, 404, authz.CodeRequestNotFound)

	// Another org's request, and the foreign desk named directly by a stranger.
	foreign := w.do(w.memberTok, w.commentsPath(w.team, strconv.Itoa(w.foreignReq.ID), ""))
	w.wantCode(foreign, 404, authz.CodeRequestNotFound)
	strangerOwnTeam := w.do(w.strangerTok, w.commentsPath(w.foreign, strconv.Itoa(w.foreignReq.ID), ""))
	if strangerOwnTeam.status != 200 {
		t.Fatalf("the foreign desk's own member = %d %v, want 200 (the tenant is the join, not a guess)", strangerOwnTeam.status, strangerOwnTeam.body)
	}

	// Another requester's request, an id that never existed and an unissued team
	// are the same bytes as the sibling's refusal.
	anothers := w.do(w.otherTok, w.commentsPath(w.team, strconv.Itoa(target.ID), ""))
	w.wantCode(anothers, 404, authz.CodeRequestNotFound)
	unknown := w.do(w.memberTok, w.commentsPath(w.team, "999999", ""))
	w.wantCode(unknown, 404, authz.CodeRequestNotFound)
	unissued := w.do(w.memberTok, w.commentsPath(999999, strconv.Itoa(target.ID), ""))
	w.wantCode(unissued, 404, authz.CodeRequestNotFound)
	if sibling.raw != unknown.raw || anothers.raw != unknown.raw || unissued.raw != unknown.raw || foreign.raw != unknown.raw {
		t.Fatalf("the refusals differ:\n sibling=%s\n anothers=%s\n unissued=%s\n foreign=%s\n unknown=%s",
			sibling.raw, anothers.raw, unissued.raw, foreign.raw, unknown.raw)
	}

	// The same refusal the detail route gives for the same request, so a client
	// cannot use the thread route to learn something the detail route hides.
	detail := w.do(w.memberTok, w.detailPath(w.team, strconv.Itoa(w.sibling.ID)))
	if detail.raw != sibling.raw {
		t.Fatalf("the detail route answers %s, the thread route %s", detail.raw, sibling.raw)
	}
	// A requester reading their own request's detail is a 200; the thread route
	// must agree about who they are rather than refusing the route outright.
	if own := w.do(w.requesterTok, w.detailPath(w.team, strconv.Itoa(target.ID))); own.status != 200 {
		t.Fatalf("requester detail = %d, want 200", own.status)
	}
	if ownThread := w.do(w.requesterTok, w.commentsPath(w.team, strconv.Itoa(target.ID), "")); ownThread.status != 200 {
		t.Fatalf("requester thread = %d %v, want 200", ownThread.status, ownThread.body)
	}

	// A submit-only credential reaches the POST it was minted for and nothing
	// else — this read included, refused by the gate rather than by a handler.
	scoped := w.do(w.scopedTok, w.commentsPath(w.team, strconv.Itoa(target.ID), ""))
	if scoped.status != 403 {
		t.Fatalf("submit-only thread read = %d %v, want 403", scoped.status, scoped.body)
	}
	if code, _ := scoped.body["code"].(string); code != authz.CodeTokenScopeDenied {
		t.Fatalf("submit-only code = %v, want %q", scoped.body["code"], authz.CodeTokenScopeDenied)
	}

	// Signed out is 401: the read never ran.
	if anon := w.do("", w.commentsPath(w.team, strconv.Itoa(target.ID), "")); anon.status != 401 {
		t.Fatalf("no token = %d, want 401", anon.status)
	}
}

// TestRESTRequestComments_PagingAndRefusals is criterion 1's paging half and the
// parameter contract: after_id/limit page the thread forward exactly once, and a
// malformed one is request.invalid naming itself.
func TestRESTRequestComments_PagingAndRefusals(t *testing.T) {
	w := newReadsWorld(t)
	target := w.mined[1]
	const total = 5
	var ids []int64
	for i := 0; i < total; i++ {
		ids = append(ids, w.postComment(w.memberTok, w.team, target.ID,
			string(domain.RequestCommentDiscussion), fmt.Sprintf("row %d", i)))
	}

	var seen []int64
	cursor := int64(0)
	for page := 0; page < 10; page++ {
		q := "limit=2"
		if cursor != 0 {
			q += "&after_id=" + strconv.FormatInt(cursor, 10)
		}
		answer := w.decodeThread(w.do(w.memberTok, w.commentsPath(w.team, strconv.Itoa(target.ID), q)))
		if answer.Count != len(answer.Comments) {
			t.Fatalf("page %d: count %d != %d rows", page, answer.Count, len(answer.Comments))
		}
		if len(answer.Comments) > 2 {
			t.Fatalf("page %d holds %d rows for limit=2", page, len(answer.Comments))
		}
		for _, c := range answer.Comments {
			seen = append(seen, c.ID)
		}
		if answer.NextAfterID == 0 {
			break
		}
		cursor = answer.NextAfterID
	}
	if len(seen) != total {
		t.Fatalf("paging yielded %d rows, want %d: %v", len(seen), total, seen)
	}
	for i, id := range seen {
		if id != ids[i] {
			t.Fatalf("paged row %d is %d, want %d: paging is not stable", i, id, ids[i])
		}
	}

	// A limit above the repository's ceiling is clamped, never refused, and the
	// page still answers the whole short thread with no cursor.
	if big := w.decodeThread(w.do(w.memberTok, w.commentsPath(w.team, strconv.Itoa(target.ID), "limit=10000"))); big.Count != total || big.NextAfterID != 0 {
		t.Fatalf("a clamped limit = %+v, want the whole thread and no cursor", big)
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
			res := w.do(w.memberTok, w.commentsPath(w.team, strconv.Itoa(target.ID), tc.query))
			w.wantCode(res, 400, authz.CodeRequestInvalid)
			if msg, _ := res.body["error"].(string); strings.TrimSpace(msg) == "" {
				t.Fatalf("%s refusal has no message", tc.name)
			}
		})
	}

	// A value that could never be a reference is 400, not a 404: the decoder is
	// the detail read's, so the two routes refuse it identically.
	bogus := w.do(w.memberTok, w.commentsPath(w.team, "not-a-reference!", ""))
	w.wantCode(bogus, 400, authz.CodeRequestInvalid)

	// A read is looking, not an act: no audit row for it, and the 404s are not
	// access events either (they are the answer for something not there for you).
	before := w.auditRows(w.member.User.ID, "request.read")
	w.do(w.memberTok, w.commentsPath(w.team, strconv.Itoa(target.ID), ""))
	if got := w.auditRows(w.member.User.ID, "request.read"); got != before {
		t.Fatalf("the thread read wrote %d request.read audit rows", got-before)
	}
}
