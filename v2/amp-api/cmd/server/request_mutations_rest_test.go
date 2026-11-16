package main

// Team request writes over REST (epic 202, story 549, AMP #3971; build slice
// F2's write half; approved contract artifact 36 v2 §1, §2 and §5).
//
// internal/service/request_mutations_test.go already proves the write path (S3,
// S4) as a boundary: who may edit, when the edit window closes, the voice table,
// the replay rules and the audit rows. internal/mcp/request_work.go's tools call
// the same five methods. So this file asks only the questions that are about *the
// routes*:
//
//	Criterion 1 — CAS and claim conflict HTTP behavior is correct:
//	  TestRESTRequestEdit_CASOverHTTP (If-Match "7" and 7 both work, a stale
//	  revision is 409 request.conflict carrying the row's actual rev, a retry
//	  with that rev succeeds, If-Match * and a weak validator are refused, and
//	  header/body disagreement is refused)
//	  TestRESTRequestClaim_ConflictNamesHolder (a claim wins, a second triager
//	  gets 409 carrying claim_user_id, and a release of someone else's claim is
//	  the same 409)
//	Criterion 2 — attribution and scope rules are tested:
//	  TestRESTRequestWrites_AuthorizationAndAttribution (the submitter edits their
//	  own untriaged request and is refused once it is claimed; a team member who
//	  did not submit is refused; a requester's question is refused while their
//	  answer is accepted; the comment answer carries no author; a stranger and a
//	  sibling team are one 404; a plain member cannot assign and a manager can)
//	  TestRESTRequestWrites_SubmitOnlyScopeIsRefused (the submit-only credential
//	  is refused on all five routes with token.scope_denied, and it is the gate
//	  that stops it)
//	Criterion 3 — bounds and coded refusals are preserved:
//	  TestRESTRequestWrites_CodedRefusals (an over-bound body, an unknown field,
//	  two JSON values, an over-long subject and a blank comment are each 400
//	  request.invalid naming the field; a reference instead of a serial is 400;
//	  a malformed team id is 400 and team 0 is the surface's 404; a refused write
//	  audits nothing and a 403 leaves one access.denied row)
//
// Every call goes through the real mux (api.RestHandler.Register, so deniedMux,
// the submit-scope gate and the denied() audit are in front of the handlers) and
// the real auth.NewAPITokenVerifier, on a scratch database per test. The tenant,
// people and requests are the reads fixture's (request_reads_rest_test.go), so
// the two files describe one world rather than two.
//
// Run against a disposable Postgres (never a live stack), per
// how-to/run-one-amp-api-test-with-disposable-postgres.md:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	  go test ./cmd/server -run 'TestRESTRequestEdit|TestRESTRequestClaim|TestRESTRequestWrites' -count=1 -race -v

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// writeResult is one write's answer: status, the decoded body and the raw bytes.
type writeResult struct {
	status int
	body   map[string]any
	raw    string
}

// send issues one request against the world's server. body is encoded as JSON
// when it is not nil; headers are set as given.
func (w *readsWorld) send(token, method, path string, body any, headers map[string]string) writeResult {
	w.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, w.srv.URL+path, reader)
	if err != nil {
		w.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := writeResult{status: resp.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// rawSend issues a request with a literal body, for the decoder's refusals (two
// JSON values, an unknown field) that a map cannot express.
func (w *readsWorld) rawSend(token, method, path, body string) writeResult {
	w.t.Helper()
	req, err := http.NewRequest(method, w.srv.URL+path, strings.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := writeResult{status: resp.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// writePath names one of the five write routes.
func (w *readsWorld) writePath(teamID int, request string, act string) string {
	p := fmt.Sprintf("/api/teams/%d/requests/%s", teamID, request)
	if act != "" {
		p += "/" + act
	}
	return p
}

// wantWriteCode asserts a refusal's status, its stable code and, when it has one,
// the extra fact a client needs (the current rev, or the claim holder).
func (w *readsWorld) wantWriteCode(r writeResult, status int, code string) {
	w.t.Helper()
	if r.status != status {
		w.t.Fatalf("%d %v, want %d %s", r.status, r.body, status, code)
	}
	if got, _ := r.body["code"].(string); got != code {
		w.t.Fatalf("code = %v, want %q (body %v)", r.body["code"], code, r.body)
	}
}

// revOf reads a request's current revision from the row.
func (w *readsWorld) revOf(requestID int) int {
	w.t.Helper()
	var rev int
	if err := w.db.QueryRow(w.ctx, `SELECT rev FROM team_requests WHERE id = $1`, requestID).Scan(&rev); err != nil {
		w.t.Fatalf("rev of %d: %v", requestID, err)
	}
	return rev
}

// commentCount counts a request's discussion rows, so "nothing was written" can
// be asserted against the table rather than against an answer.
func (w *readsWorld) commentCount(requestID int) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRow(w.ctx, `SELECT count(*) FROM request_comments WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		w.t.Fatalf("count comments of %d: %v", requestID, err)
	}
	return n
}

// subjectOf reads a request's stored subject.
func (w *readsWorld) subjectOf(requestID int) string {
	w.t.Helper()
	var subject string
	if err := w.db.QueryRow(w.ctx, `SELECT subject FROM team_requests WHERE id = $1`, requestID).Scan(&subject); err != nil {
		w.t.Fatalf("subject of %d: %v", requestID, err)
	}
	return subject
}

// answerFacts reads the {id, public_ref, state, rev} envelope a write answers
// with, and asserts nothing else came with it.
//
// The answer is a whitelist and not the record, so the check is exact: the four
// keys are all a write says about the request. `rev` is deliberately among them
// (a compare-and-set client needs it to save again) while every other internal
// column — the org, the claim, the assignee, the contact snapshot, the metadata,
// the reject reason — has no key here at all.
func (w *readsWorld) answerFacts(r writeResult) (id int, publicRef string, rev int, changed bool) {
	w.t.Helper()
	req, ok := r.body["request"].(map[string]any)
	if !ok {
		w.t.Fatalf("a write answered no request facts: %v", r.body)
	}
	allowed := map[string]bool{"id": true, "public_ref": true, "state": true, "rev": true}
	for key := range req {
		if !allowed[key] {
			w.t.Fatalf("a write answer carries %q, which is not one of the four facts it may say: %v", key, req)
		}
	}
	idf, _ := req["id"].(float64)
	revf, _ := req["rev"].(float64)
	publicRef, _ = req["public_ref"].(string)
	changed, _ = r.body["changed"].(bool)
	return int(idf), publicRef, int(revf), changed
}

// ---- criterion 1: compare-and-set and claim conflicts over HTTP ----

// TestRESTRequestEdit_CASOverHTTP: the revision is the HTTP precondition. A
// quoted entity tag and a bare number both name it; a stale one is a 409 that
// carries the revision the row is actually at, so the retry needs no extra read;
// and the two spellings a client might use to opt out of the compare-and-set
// (`*` and a weak validator) are refused rather than honoured.
func TestRESTRequestEdit_CASOverHTTP(t *testing.T) {
	w := newReadsWorld(t)
	// r2 is the second customer's request, still untriaged: the edit's window.
	target := w.mined[2]
	path := w.writePath(w.team, strconv.Itoa(target.ID), "")
	rev := w.revOf(target.ID)

	// A quoted If-Match names the revision, and the edit lands.
	quoted := w.send(w.otherTok, http.MethodPatch, path,
		map[string]any{"subject": "Please add a dark theme, with contrast"}, map[string]string{"If-Match": `"` + strconv.Itoa(rev) + `"`})
	if quoted.status != 200 {
		t.Fatalf("edit with a quoted If-Match = %d %v, want 200", quoted.status, quoted.body)
	}
	id, publicRef, newRev, changed := w.answerFacts(quoted)
	if id != target.ID || publicRef != target.PublicRef || !changed {
		t.Fatalf("edit answer = %d/%q/%v, want the request's own facts and changed", id, publicRef, changed)
	}
	if newRev != rev+1 {
		t.Fatalf("edit left rev at %d, want %d", newRev, rev+1)
	}
	if got := w.subjectOf(target.ID); got != "Please add a dark theme, with contrast" {
		t.Fatalf("stored subject = %q, want the edit's", got)
	}

	// The same revision again is stale: 409 request.conflict, and the body
	// carries the revision the row is at — what the retry needs.
	stale := w.send(w.otherTok, http.MethodPatch, path,
		map[string]any{"subject": "a second thought"}, map[string]string{"If-Match": `"` + strconv.Itoa(rev) + `"`})
	w.wantWriteCode(stale, 409, authz.CodeRequestConflict)
	if got, _ := stale.body["rev"].(float64); int(got) != newRev {
		t.Fatalf("the conflict reports rev %v, want the row's %d", stale.body["rev"], newRev)
	}
	if msg, _ := stale.body["error"].(string); !strings.Contains(msg, strconv.Itoa(newRev)) {
		t.Fatalf("the conflict's message does not name the current rev: %q", msg)
	}
	if got := w.subjectOf(target.ID); got != "Please add a dark theme, with contrast" {
		t.Fatalf("a refused edit changed the subject to %q", got)
	}

	// A retry with the revision the conflict reported succeeds — no re-read.
	retry := w.send(w.otherTok, http.MethodPatch, path,
		map[string]any{"subject": "a second thought"}, map[string]string{"If-Match": strconv.Itoa(newRev)})
	if retry.status != 200 {
		t.Fatalf("retry with the reported rev = %d %v, want 200 (and a bare number is a revision too)", retry.status, retry.body)
	}

	// `If-Match: *` is refused, and the refusal names the header — it must not be
	// read as "no precondition given", because a caller who sent it believes the
	// write is guarded, and `*` means "any representation": precisely the lost
	// update the revision exists to prevent.
	star := w.send(w.otherTok, http.MethodPatch, path,
		map[string]any{"subject": "whatever"}, map[string]string{"If-Match": "*"})
	w.wantWriteCode(star, 400, authz.CodeRequestInvalid)
	starMsg, _ := star.body["error"].(string)
	if !strings.Contains(starMsg, "*") {
		t.Fatalf("the `*` refusal does not name what was wrong with it: %q", starMsg)
	}
	if strings.Contains(starMsg, "expected_rev is required") {
		t.Fatalf("If-Match: * was read as no precondition at all: %q", starMsg)
	}
	if got := w.subjectOf(target.ID); got != "a second thought" {
		t.Fatalf("an If-Match: * edit landed: subject = %q", got)
	}
	// A weak validator is refused too: If-Match is a strong comparison.
	weak := w.send(w.otherTok, http.MethodPatch, path,
		map[string]any{"subject": "whatever"}, map[string]string{"If-Match": `W/"3"`})
	w.wantWriteCode(weak, 400, authz.CodeRequestInvalid)
	weakMsg, _ := weak.body["error"].(string)
	if !strings.Contains(weakMsg, "If-Match") || strings.Contains(weakMsg, "expected_rev is required") {
		t.Fatalf("the weak-validator refusal reads as a missing precondition: %q", weakMsg)
	}
	// Two spellings that disagree are a caller bug, not a choice to make for them.
	disagree := w.send(w.otherTok, http.MethodPatch, path,
		map[string]any{"subject": "whatever", "expected_rev": 99}, map[string]string{"If-Match": "3"})
	w.wantWriteCode(disagree, 400, authz.CodeRequestInvalid)
	if msg, _ := disagree.body["error"].(string); !strings.Contains(msg, "disagree") {
		t.Fatalf("the disagreement refusal does not say the two disagree: %q", msg)
	}
	// Header and body that agree are one precondition, and they work.
	agreeRev := w.revOf(target.ID)
	agree := w.send(w.otherTok, http.MethodPatch, path,
		map[string]any{"subject": "agreed", "expected_rev": agreeRev}, map[string]string{"If-Match": strconv.Itoa(agreeRev)})
	if agree.status != 200 {
		t.Fatalf("header and body agreeing = %d %v, want 200", agree.status, agree.body)
	}
	if got := w.subjectOf(target.ID); got != "agreed" {
		t.Fatalf("stored subject = %q, want the agreed edit", got)
	}
}

// TestRESTRequestClaim_ConflictNamesHolder: a claim is the caller's, and a second
// triager loses with a 409 that names who holds it — the fact a UI needs to say
// "Alice is on it". Releasing someone else's claim is the same refusal, because
// the only legitimate way to move work is an assignment or the holder's release.
func TestRESTRequestClaim_ConflictNamesHolder(t *testing.T) {
	w := newReadsWorld(t)
	target := w.mined[0] // assigned to the member, unclaimed: claimable
	// Clear the assignee so the claim is the only thing in play.
	if _, err := w.db.Exec(w.ctx, `UPDATE team_requests SET owner_user_id = NULL WHERE id = $1`, target.ID); err != nil {
		t.Fatalf("clear the assignee: %v", err)
	}
	path := w.writePath(w.team, strconv.Itoa(target.ID), "claim")

	won := w.send(w.memberTok, http.MethodPost, path, nil, nil)
	if won.status != 200 {
		t.Fatalf("claim = %d %v, want 200", won.status, won.body)
	}
	_, _, wonRev, changed := w.answerFacts(won)
	if !changed {
		t.Fatalf("the first claim reported changed=false")
	}
	if wonRev < 2 {
		t.Fatalf("the claim left rev at %d; a claim bumps it", wonRev)
	}

	// The org owner is a triager too, and loses to the member's claim. The body
	// names the holder — a user id, which is the fact a triage UI renders.
	lost := w.send(w.adminTokOrFail(t), http.MethodPost, path, nil, nil)
	w.wantWriteCode(lost, 409, authz.CodeRequestConflict)
	holder, _ := lost.body["claim_user_id"].(float64)
	if int(holder) != w.member.User.ID {
		t.Fatalf("the conflict names claimant %v, want the member %d", lost.body["claim_user_id"], w.member.User.ID)
	}

	// A release of somebody else's claim is refused the same way, and the claim
	// survives.
	steal := w.send(w.adminTokOrFail(t), http.MethodPost, w.writePath(w.team, strconv.Itoa(target.ID), "release"), nil, nil)
	w.wantWriteCode(steal, 409, authz.CodeRequestConflict)
	var stillClaimed *int
	if err := w.db.QueryRow(w.ctx, `SELECT claim_user_id FROM team_requests WHERE id = $1`, target.ID).Scan(&stillClaimed); err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	if stillClaimed == nil || *stillClaimed != w.member.User.ID {
		t.Fatalf("a refused release moved the claim: %v", stillClaimed)
	}

	// The holder's own release ends it, and a repeat is a silent no-op with the
	// revision unmoved — a retry after a lost response is safe.
	released := w.send(w.memberTok, http.MethodPost, w.writePath(w.team, strconv.Itoa(target.ID), "release"), nil, nil)
	if released.status != 200 {
		t.Fatalf("release = %d %v, want 200", released.status, released.body)
	}
	_, _, afterRelease, wasChanged := w.answerFacts(released)
	if !wasChanged {
		t.Fatalf("the release reported changed=false")
	}
	again := w.send(w.memberTok, http.MethodPost, w.writePath(w.team, strconv.Itoa(target.ID), "release"), nil, nil)
	if again.status != 200 {
		t.Fatalf("repeat release = %d %v, want 200", again.status, again.body)
	}
	_, _, afterRepeat, changedAgain := w.answerFacts(again)
	if changedAgain {
		t.Fatalf("a repeat release reported changed=true")
	}
	if afterRepeat != afterRelease {
		t.Fatalf("a repeat release bumped rev from %d to %d", afterRelease, afterRepeat)
	}

	// A claim with a stale revision is a conflict carrying the current rev, not
	// a silent success. The revision is a positive one that is simply out of
	// date: a zero or negative value can name no stored row (rev starts at 1), so
	// it is the caller's payload mistake and request.invalid instead.
	stale := w.send(w.memberTok, http.MethodPost, path, map[string]any{"expected_rev": 1}, nil)
	w.wantWriteCode(stale, 409, authz.CodeRequestConflict)
	if got, _ := stale.body["rev"].(float64); int(got) != afterRepeat {
		t.Fatalf("the stale claim reports rev %v, want %d", stale.body["rev"], afterRepeat)
	}
}

// ---- criterion 2: attribution, scope and the view rules ----

// TestRESTRequestWrites_AuthorizationAndAttribution walks the rules a surface can
// get wrong that the service cannot: whose words an edit is, when the edit window
// closes, which voice a discussion row speaks in, what the answer says about the
// author, and which people a request is even visible to.
func TestRESTRequestWrites_AuthorizationAndAttribution(t *testing.T) {
	w := newReadsWorld(t)

	// The submitter edits their own untriaged request: r2 belongs to the second
	// customer, and the answer is the request's facts (never the record).
	target := w.mined[2]
	edit := w.send(w.otherTok, http.MethodPatch, w.writePath(w.team, strconv.Itoa(target.ID), ""),
		map[string]any{"subject": "Dark theme, please", "expected_rev": w.revOf(target.ID)}, nil)
	if edit.status != 200 {
		t.Fatalf("submitter edit = %d %v, want 200", edit.status, edit.body)
	}
	// A team member who did not submit it is refused — with 403, not 404, because
	// they can already see the request (the service's ordering).
	memberEdit := w.send(w.memberTok, http.MethodPatch, w.writePath(w.team, strconv.Itoa(target.ID), ""),
		map[string]any{"subject": "someone else's words", "expected_rev": w.revOf(target.ID)}, nil)
	w.wantWriteCode(memberEdit, 403, "")

	// A claim closes the edit window: the content is fixed once the desk has
	// picked the request up.
	if res := w.send(w.memberTok, http.MethodPost, w.writePath(w.team, strconv.Itoa(target.ID), "claim"), nil, nil); res.status != 200 {
		t.Fatalf("claim = %d %v", res.status, res.body)
	}
	afterClaim := w.send(w.otherTok, http.MethodPatch, w.writePath(w.team, strconv.Itoa(target.ID), ""),
		map[string]any{"subject": "too late", "expected_rev": w.revOf(target.ID)}, nil)
	w.wantWriteCode(afterClaim, 403, "")
	if got := w.subjectOf(target.ID); got != "Dark theme, please" {
		t.Fatalf("an edit landed after the claim: subject = %q", got)
	}

	// A requester's `answer` is theirs; a `question` is the desk's and is refused
	// from the other side (403, a coded refusal, nothing written).
	commentPath := w.writePath(w.team, strconv.Itoa(target.ID), "comments")
	answer := w.send(w.otherTok, http.MethodPost, commentPath, map[string]any{"kind": "answer", "body": "Yes, please."}, nil)
	if answer.status != 201 {
		t.Fatalf("requester answer = %d %v, want 201", answer.status, answer.body)
	}
	// The answer carries no author: a requester is never handed a triager's name,
	// and this row could have been a retry answered with someone else's.
	row, ok := answer.body["comment"].(map[string]any)
	if !ok {
		t.Fatalf("the comment answer has no row: %v", answer.body)
	}
	for _, key := range []string{"author", "author_user_id", "request_id"} {
		if _, leaked := row[key]; leaked {
			t.Fatalf("the comment answer carries %q: %v", key, row)
		}
	}
	if wrote, _ := answer.body["wrote"].(bool); !wrote {
		t.Fatalf("a new comment reported wrote=false")
	}
	badVoice := w.send(w.otherTok, http.MethodPost, commentPath, map[string]any{"kind": "question", "body": "Why?"}, nil)
	w.wantWriteCode(badVoice, 403, "")

	// A retry naming the row it wrote answers that row (200, wrote=false) and
	// writes nothing.
	commentID, _ := row["id"].(float64)
	retried := w.send(w.otherTok, http.MethodPost, commentPath,
		map[string]any{"body": "Yes, please.", "retry_of_comment_id": int64(commentID)}, nil)
	if retried.status != 200 {
		t.Fatalf("a comment retry = %d %v, want 200", retried.status, retried.body)
	}
	if wrote, _ := retried.body["wrote"].(bool); wrote {
		t.Fatalf("a comment retry reported wrote=true")
	}

	// A stranger, a sibling team's request read through this team's path, and an
	// id that never existed are one answer, byte for byte.
	stranger := w.send(w.strangerTok, http.MethodPost, w.writePath(w.team, strconv.Itoa(target.ID), "claim"), nil, nil)
	w.wantWriteCode(stranger, 404, authz.CodeRequestNotFound)
	sibling := w.send(w.memberTok, http.MethodPost, w.writePath(w.team, strconv.Itoa(w.sibling.ID), "claim"), nil, nil)
	w.wantWriteCode(sibling, 404, authz.CodeRequestNotFound)
	unknown := w.send(w.memberTok, http.MethodPost, w.writePath(w.team, "999999", "claim"), nil, nil)
	w.wantWriteCode(unknown, 404, authz.CodeRequestNotFound)
	if stranger.raw != unknown.raw || sibling.raw != unknown.raw {
		t.Fatalf("404 bodies differ:\n stranger=%s sibling=%s unknown=%s", stranger.raw, sibling.raw, unknown.raw)
	}

	// Assignment is a manager's act: a plain member is refused, the org owner is
	// not, and the transfer clears the claim in the same write.
	assignPath := w.writePath(w.team, strconv.Itoa(target.ID), "assign")
	memberAssign := w.send(w.memberTok, http.MethodPost, assignPath, map[string]any{"assignee": strconv.Itoa(w.member.User.ID)}, nil)
	w.wantWriteCode(memberAssign, 403, "")
	managerAssign := w.send(w.adminTokOrFail(t), http.MethodPost, assignPath,
		map[string]any{"assignee": strconv.Itoa(w.member.User.ID)}, nil)
	if managerAssign.status != 200 {
		t.Fatalf("manager assign = %d %v, want 200", managerAssign.status, managerAssign.body)
	}
	var owner, claim *int
	if err := w.db.QueryRow(w.ctx, `SELECT owner_user_id, claim_user_id FROM team_requests WHERE id = $1`, target.ID).Scan(&owner, &claim); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if owner == nil || *owner != w.member.User.ID {
		t.Fatalf("assignee = %v, want the member %d", owner, w.member.User.ID)
	}
	if claim != nil {
		t.Fatalf("the transfer left claim %v in place", *claim)
	}
	// Unassigning is a real operation, not a no-op guard.
	unassign := w.send(w.adminTokOrFail(t), http.MethodPost, assignPath, map[string]any{"assignee": "none"}, nil)
	if unassign.status != 200 {
		t.Fatalf("unassign = %d %v, want 200", unassign.status, unassign.body)
	}
	if err := w.db.QueryRow(w.ctx, `SELECT owner_user_id FROM team_requests WHERE id = $1`, target.ID).Scan(&owner); err != nil {
		t.Fatalf("read the unassignment: %v", err)
	}
	if owner != nil {
		t.Fatalf("unassign left owner %v", *owner)
	}

	// Every accepted act was audited once, by the service, under its own action.
	for action, want := range map[string]int{"request.update": 1, "request.claim": 1, "request.comment": 1, "request.assign": 2, "request.release": 0} {
		if got := w.auditRows(w.member.User.ID, action) + w.auditRows(w.admin.User.ID, action) + w.auditRows(w.other.User.ID, action); got != want {
			t.Fatalf("%s audit rows = %d, want %d", action, got, want)
		}
	}
}

// TestRESTRequestWrites_SubmitOnlyScopeIsRefused: a submit-only service
// credential may POST a submission and nothing else. It is refused on all five
// write routes with token.scope_denied, and the refusal comes from the gate in
// front of the handler — the same one the read routes use — so a route added
// tomorrow cannot inherit write access by forgetting a check.
func TestRESTRequestWrites_SubmitOnlyScopeIsRefused(t *testing.T) {
	w := newReadsWorld(t)
	target := w.mined[0]
	revBefore := w.revOf(target.ID)
	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"edit", http.MethodPatch, w.writePath(w.team, strconv.Itoa(target.ID), ""), map[string]any{"subject": "x", "expected_rev": 1}},
		{"comment", http.MethodPost, w.writePath(w.team, strconv.Itoa(target.ID), "comments"), map[string]any{"body": "x"}},
		{"claim", http.MethodPost, w.writePath(w.team, strconv.Itoa(target.ID), "claim"), nil},
		{"release", http.MethodPost, w.writePath(w.team, strconv.Itoa(target.ID), "release"), nil},
		{"assign", http.MethodPost, w.writePath(w.team, strconv.Itoa(target.ID), "assign"), map[string]any{"assignee": "me"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := w.send(w.scopedTok, c.method, c.path, c.body, nil)
			if res.status != 403 {
				t.Fatalf("%s = %d %v, want 403", c.name, res.status, res.body)
			}
			if code, _ := res.body["code"].(string); code != authz.CodeTokenScopeDenied {
				t.Fatalf("%s code = %v, want %q", c.name, res.body["code"], authz.CodeTokenScopeDenied)
			}
		})
	}
	// The submission POST on the same path prefix still works for the same
	// credential: the gate refuses writes, not the one operation it exists for.
	submit := w.send(w.scopedTok, http.MethodPost, fmt.Sprintf("/api/teams/%d/requests", w.team),
		map[string]any{"subject": "A scoped submission " + uniq("sc")}, nil)
	if submit.status != 201 {
		t.Fatalf("scoped submit = %d %v, want 201", submit.status, submit.body)
	}
	// And nothing moved on the request the credential tried to write.
	if got := w.revOf(target.ID); got != revBefore {
		t.Fatalf("a submit-only credential moved the request's rev from %d to %d", revBefore, got)
	}
}

// ---- criterion 3: bounds and coded refusals ----

// TestRESTRequestWrites_CodedRefusals pins the transport's error contract: a body
// that cannot be a write is 400 request.invalid naming what is wrong, a malformed
// team id is a plain 400 (not a request.* code), team 0 is the surface's 404, and
// a refused write writes no audit row of its own — except a 403, which is the
// access.denied the denied() wrapper records.
func TestRESTRequestWrites_CodedRefusals(t *testing.T) {
	w := newReadsWorld(t)
	// A fresh request of the requester's own, untriaged and unclaimed: the edit
	// path's authorization is the submitter's while the request is still theirs,
	// so the field refusals below have to be reached as that person. (The
	// authorization itself is TestRESTRequestWrites_AuthorizationAndAttribution's
	// subject; this test is about the payload.)
	fresh, err := w.svc.SubmitRequest(w.ctx, w.principal(w.requester.User.ID), service.SubmitRequestInput{
		TeamID: w.team, Source: domain.RequestSourceWeb, Subject: "A request to edit " + uniq("cr"),
	})
	if err != nil {
		t.Fatalf("submit the editable request: %v", err)
	}
	editPath := w.writePath(w.team, strconv.Itoa(fresh.Request.ID), "")
	editRev := w.revOf(fresh.Request.ID)

	target := w.mined[0]
	path := w.writePath(w.team, strconv.Itoa(target.ID), "")
	before := w.auditRows(w.member.User.ID, "request.update") + w.auditRows(w.member.User.ID, "request.comment") +
		w.auditRows(w.member.User.ID, "request.claim") + w.auditRows(w.member.User.ID, "request.release") +
		w.auditRows(w.member.User.ID, "request.assign")

	cases := []struct {
		name   string
		token  string
		method string
		path   string
		body   string
		code   string
	}{
		{"unknown field on an edit", w.requesterTok, http.MethodPatch, editPath, `{"subject":"x","expected_rev":` + strconv.Itoa(editRev) + `,"state":"delivered"}`, authz.CodeRequestInvalid},
		{"an edit cannot name its actor", w.requesterTok, http.MethodPatch, editPath, `{"subject":"x","expected_rev":` + strconv.Itoa(editRev) + `,"actor":"someone"}`, authz.CodeRequestInvalid},
		{"two JSON values", w.requesterTok, http.MethodPatch, editPath, `{"subject":"x","expected_rev":` + strconv.Itoa(editRev) + `}{"subject":"y"}`, authz.CodeRequestInvalid},
		{"an over-long subject", w.requesterTok, http.MethodPatch, editPath, `{"subject":"` + strings.Repeat("s", domain.MaxRequestSubjectLen+1) + `","expected_rev":` + strconv.Itoa(editRev) + `}`, authz.CodeRequestInvalid},
		{"a blank subject", w.requesterTok, http.MethodPatch, editPath, `{"subject":"   ","expected_rev":` + strconv.Itoa(editRev) + `}`, authz.CodeRequestInvalid},
		{"a non-positive expected_rev", w.requesterTok, http.MethodPatch, editPath, `{"subject":"x","expected_rev":0}`, authz.CodeRequestInvalid},
		{"an edit with no revision at all", w.requesterTok, http.MethodPatch, editPath, `{"subject":"x"}`, authz.CodeRequestInvalid},
		{"an edit naming outcome_brief", w.requesterTok, http.MethodPatch, editPath, `{"outcome_brief":"we did it","expected_rev":` + strconv.Itoa(editRev) + `}`, authz.CodeRequestInvalid},
		{"an edit naming metadata", w.requesterTok, http.MethodPatch, editPath, `{"metadata":{"jira":"X-1"},"expected_rev":` + strconv.Itoa(editRev) + `}`, authz.CodeRequestInvalid},
		{"an unknown comment kind", w.memberTok, http.MethodPost, path + "/comments", `{"kind":"shout","body":"hello"}`, authz.CodeRequestInvalid},
		{"a blank comment body", w.memberTok, http.MethodPost, path + "/comments", `{"body":"   "}`, authz.CodeRequestInvalid},
		{"an over-long comment body", w.memberTok, http.MethodPost, path + "/comments", `{"body":"` + strings.Repeat("c", domain.MaxRequestBriefLen+1) + `"}`, authz.CodeRequestInvalid},
		{"a claim naming a claimant", w.memberTok, http.MethodPost, path + "/claim", `{"claim_user_id":1}`, authz.CodeRequestInvalid},
		{"a claim naming a state", w.memberTok, http.MethodPost, path + "/claim", `{"state":"delivered"}`, authz.CodeRequestInvalid},
		{"an assignment with no assignee", w.adminTokOrFail(t), http.MethodPost, path + "/assign", `{}`, authz.CodeRequestInvalid},
		{"a release naming a holder", w.memberTok, http.MethodPost, path + "/release", `{"claim_user_id":1}`, authz.CodeRequestInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := w.rawSend(c.token, c.method, c.path, c.body)
			if res.status != 400 {
				t.Fatalf("%s = %d %v, want 400", c.name, res.status, res.body)
			}
			if code, _ := res.body["code"].(string); code != c.code {
				t.Fatalf("%s code = %v, want %q (body %v)", c.name, res.body["code"], c.code, res.body)
			}
			if msg, _ := res.body["error"].(string); strings.TrimSpace(msg) == "" {
				t.Fatalf("%s refusal has no message", c.name)
			}
		})
	}

	// An assignment naming a user who does not exist is the manager's answer about
	// the user, not about the request: a plain 404 that does NOT carry
	// request.not_found, so a client cannot read it as "the request is gone".
	// (Only a team manager reaches this branch at all, so it tells a triager
	// nothing they could not learn from the member list.)
	unknownUser := w.rawSend(w.adminTokOrFail(t), http.MethodPost, path+"/assign",
		`{"assignee":"nobody-`+uniq("x")+`@nowhere.test"}`)
	if unknownUser.status != 404 {
		t.Fatalf("an assignment naming an unknown user = %d %v, want 404", unknownUser.status, unknownUser.body)
	}
	if code, _ := unknownUser.body["code"].(string); code != "" {
		t.Fatalf("an unknown assignee carries the code %q; it must not read as request.not_found", code)
	}

	// A customer with only a submit grant cannot be handed the request: they
	// could not open it, so the service refuses with request.invalid naming why.
	customerAssign := w.rawSend(w.adminTokOrFail(t), http.MethodPost, path+"/assign",
		`{"assignee":"`+w.requester.Email+`"}`)
	w.wantWriteCode(customerAssign, 400, authz.CodeRequestInvalid)

	// An over-bound body is refused before it is read — and the assertion is a
	// body whose FIELDS are all legal, so nothing but the transport's own bound can
	// refuse it: the domain would have accepted this comment. The message names the
	// bound, and no row is written.
	commentsBefore := w.commentCount(target.ID)
	overBody := `{"body":"within every domain bound"}` + strings.Repeat(" ", 300<<10)
	oversized := w.rawSend(w.memberTok, http.MethodPost, path+"/comments", overBody)
	w.wantWriteCode(oversized, 400, authz.CodeRequestInvalid)
	if msg, _ := oversized.body["error"].(string); !strings.Contains(msg, "over") || !strings.Contains(msg, "bytes") {
		t.Fatalf("the over-bound refusal does not name the bound: %q", msg)
	}
	if got := w.commentCount(target.ID); got != commentsBefore {
		t.Fatalf("an over-bound body wrote %d comment rows", got-commentsBefore)
	}

	// A write addresses the request by its serial. The public reference is the
	// reads' spelling and is refused here by name rather than searched for.
	byRef := w.send(w.adminTokOrFail(t), http.MethodPost, w.writePath(w.team, "REQ-"+target.PublicRef, "claim"), nil, nil)
	w.wantWriteCode(byRef, 400, authz.CodeRequestInvalid)
	// A value that is neither is the same refusal.
	junk := w.send(w.adminTokOrFail(t), http.MethodPost, w.writePath(w.team, "not-a-serial!", "claim"), nil, nil)
	w.wantWriteCode(junk, 400, authz.CodeRequestInvalid)

	// A malformed team id is a malformed URL (a plain 400, no request.* code);
	// team 0 is the surface's 404, because a 400 would let a caller tell "team 0"
	// from "a team that isn't yours".
	badTeam := w.send(w.adminTokOrFail(t), http.MethodPost, "/api/teams/not-a-number/requests/1/claim", nil, nil)
	if badTeam.status != 400 {
		t.Fatalf("bad team id = %d %v, want 400", badTeam.status, badTeam.body)
	}
	if code, _ := badTeam.body["code"].(string); code != "" {
		t.Fatalf("a malformed team id carries a request.* code %q", code)
	}
	zeroTeam := w.send(w.adminTokOrFail(t), http.MethodPost, "/api/teams/0/requests/1/claim", nil, nil)
	w.wantWriteCode(zeroTeam, 404, authz.CodeRequestNotFound)

	// A method the path does not take is refused by the router, not answered as
	// a different act: GET on /claim is a read of a path that has none.
	if wrong := w.send(w.adminTokOrFail(t), http.MethodGet, path+"/claim", nil, nil); wrong.status != 405 && wrong.status != 404 {
		t.Fatalf("GET on a write path = %d %v, want 404/405", wrong.status, wrong.body)
	}

	// Nothing above wrote a request.* audit row: a refused write is not an act.
	after := w.auditRows(w.member.User.ID, "request.update") + w.auditRows(w.member.User.ID, "request.comment") +
		w.auditRows(w.member.User.ID, "request.claim") + w.auditRows(w.member.User.ID, "request.release") +
		w.auditRows(w.member.User.ID, "request.assign")
	if after != before {
		t.Fatalf("refused writes produced %d request.* audit rows, want none", after-before)
	}
}

// TestRESTRequestWrites_NoLifecycleOrAcceptanceRoute is the negative boundary: the
// surface this ticket adds is the five mutations and nothing else. There is no
// delivery, acceptance, answer-transition, reopen, attach or attachment route —
// each is another slice's — and a POST to one of those paths is not silently
// treated as a claim.
func TestRESTRequestWrites_NoLifecycleOrAcceptanceRoute(t *testing.T) {
	w := newReadsWorld(t)
	target := w.mined[0]
	rev := w.revOf(target.ID)
	for _, act := range []string{"accept", "deliver", "reject", "duplicate", "reopen", "answer", "transition", "attach", "attachments", "links"} {
		res := w.send(w.adminTokOrFail(t), http.MethodPost, w.writePath(w.team, strconv.Itoa(target.ID), act), map[string]any{"x": "y"}, nil)
		if res.status == 200 || res.status == 201 {
			t.Fatalf("POST …/%s answered %d: this slice exposes no such act", act, res.status)
		}
	}
	if got := w.revOf(target.ID); got != rev {
		t.Fatalf("a refused route moved rev from %d to %d", rev, got)
	}
	var state string
	if err := w.db.QueryRow(w.ctx, `SELECT state FROM team_requests WHERE id = $1`, target.ID).Scan(&state); err != nil {
		t.Fatalf("read the state: %v", err)
	}
	if state != string(domain.RequestStatusNew) {
		t.Fatalf("a refused route moved the request to %q", state)
	}
}
