package main

// The inbox read and triage tools through the real MCP server (epic 202, story
// 549, AMP #3969; approved contract artifact 36 v2 §1/§2/§5; build slice I2).
//
// The seven tools are thin adapters over service methods that
// internal/service/request_views_test.go and request_mutations_test.go already
// prove as boundaries. So this file asks only what is about *the tools*:
//
//   - does the call arrive at the same service, and therefore produce the same
//     answer REST's adapter will (a queue page with server counts, a team record
//     versus a requester projection, an audited write with its history row)?
//   - does the view matrix hold through this transport: a triager reads the
//     queue, a granted requester does not, a stranger learns nothing, and an
//     unknown id is the same bytes as another team's id?
//   - do the CAS rules arrive intact: a stale expected_rev is 409
//     request.conflict naming the revision, a lost claim race is 409 naming the
//     holder, and a replay writes nothing?
//   - is the negative boundary real: a submit-only credential is refused every
//     one of these tools, no tool can accept a delivery or move a state, and
//     there is no attachment or planning tool here?
//
// Criterion 1 — tool authorization/CAS parity tested:
//   TestMCPRequestWork_ViewMatrix, TestMCPRequestWork_EditCAS,
//   TestMCPRequestWork_ClaimRaceAndReplay, TestMCPRequestWork_ReleaseAndAssign,
//   TestMCPRequestWork_Discussion
// Criterion 2 — strict schema/flags correct:
//   TestMCPRequestWork_SchemaAndCatalog (the wire half; the unit half is
//   internal/mcp/request_work_test.go)
// Criterion 3 — submit-only and acceptance boundaries preserved:
//   TestMCPRequestWork_SubmitOnlyCredentialIsRefused,
//   TestMCPRequestWork_NoAcceptanceOrLifecycleTool
//
// Every call goes through the same path an agent's does: JSON-RPC → MCP server
// (its real middleware included) → tool handler → service → postgres. Identities
// are built in process from users rows, exactly as the auth middleware builds
// them; no live credential and no live stack is involved.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/mcp"
)

// requestWorkToolNames is the surface this slice ships, spelled out here so the
// end-to-end assertions (listed, scope-refused, no acceptance among them) read
// against a fixed list rather than against whatever happens to be registered.
var requestWorkToolNames = []string{
	"amp_list_requests",
	"amp_get_request",
	"amp_update_request",
	"amp_comment_request",
	"amp_claim_request",
	"amp_release_request",
	"amp_assign_request",
}

// requestWorkWorld is the submission fixture plus the two things triage needs: a
// request to work on, and a second one in a sibling team the caller can also see
// (so "not this team's request" is tested against a request that exists).
type requestWorkWorld struct {
	*submitMCPWorld
	ref      string // a request in w.team, submitted by the member
	refID    int    // its serial, as a queue row shows it
	otherRef string // a request in w.otherTeam, submitted by the org owner
}

func newRequestWorkWorld(t *testing.T) *requestWorkWorld {
	t.Helper()
	w := &requestWorkWorld{submitMCPWorld: newSubmitMCPWorld(t)}

	// A request in the inbox team, filed by the team's own member.
	sub := w.callTool(t, w.member, mcp.SubmitScopeTool, submitArgs(w.team, "queue export "+uniq("rw"), ""))
	if sub.errMessage != "" {
		t.Fatalf("fixture submission refused: %s %v", sub.errMessage, sub.errData)
	}
	w.ref = requestField(t, sub, "public_ref").(string)
	w.refID = w.stored(t, w.ref).ID

	// A request in the sibling team. The org owner may submit there (owner of
	// the org is a submit principal for any team in it), so this exercises "an
	// id from another team" as a real row rather than an invented id.
	other := w.callTool(t, w.admin, mcp.SubmitScopeTool, submitArgs(w.otherTeam, "sibling queue "+uniq("rw"), ""))
	if other.errMessage != "" {
		t.Fatalf("sibling submission refused: %s %v", other.errMessage, other.errData)
	}
	w.otherRef = requestField(t, other, "public_ref").(string)
	return w
}

// tool runs one of this slice's tools as who, never failing the test on a
// refusal (a refusal is a result: the matrix asserts both halves).
func (w *requestWorkWorld) tool(t *testing.T, who *auth.Identity, name string, args map[string]interface{}) mcpCallResult {
	t.Helper()
	return w.callTool(t, who, name, args)
}

// mustTool runs a tool expecting success and returns its payload.
func (w *requestWorkWorld) mustTool(t *testing.T, who *auth.Identity, name string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	got := w.tool(t, who, name, args)
	if got.errMessage != "" {
		t.Fatalf("%s refused: %s %v", name, got.errMessage, got.errData)
	}
	return got.result
}

// wantRefusal asserts a coded refusal: the status and code REST would answer
// with, in the error's data.
func wantWorkRefusal(t *testing.T, what string, got mcpCallResult, status int, code string) {
	t.Helper()
	if got.errMessage == "" {
		t.Fatalf("%s was allowed: %v", what, got.result)
	}
	if got.errData["status"] != float64(status) {
		t.Fatalf("%s status = %v, want %d (%s)", what, got.errData["status"], status, got.errMessage)
	}
	if code != "" && got.errData["code"] != code {
		t.Fatalf("%s code = %v, want %q (%s)", what, got.errData["code"], code, got.errMessage)
	}
}

// request rev read straight from the row: the number a caller's expected_rev
// has to match, and the one a conflict names.
func (w *requestWorkWorld) rev(t *testing.T, id int) int {
	t.Helper()
	var rev int
	if err := w.db(t).QueryRow(context.Background(), `SELECT rev FROM team_requests WHERE id = $1`, id).Scan(&rev); err != nil {
		t.Fatalf("rev of request %d: %v", id, err)
	}
	return rev
}

// stateOf reads a request's stored lifecycle state.
func (w *requestWorkWorld) stateOf(t *testing.T, id int) string {
	t.Helper()
	var state string
	if err := w.db(t).QueryRow(context.Background(), `SELECT state FROM team_requests WHERE id = $1`, id).Scan(&state); err != nil {
		t.Fatalf("state of request %d: %v", id, err)
	}
	return state
}

// claimOf reads a request's stored claim and assignee.
func (w *requestWorkWorld) claimOf(t *testing.T, id int) (claim, owner *int) {
	t.Helper()
	if err := w.db(t).QueryRow(context.Background(),
		`SELECT claim_user_id, owner_user_id FROM team_requests WHERE id = $1`, id).Scan(&claim, &owner); err != nil {
		t.Fatalf("claim of request %d: %v", id, err)
	}
	return claim, owner
}

// historyActions is the request's append-only history, newest last.
func (w *requestWorkWorld) historyActions(t *testing.T, id int) []string {
	t.Helper()
	rows, err := w.db(t).Query(context.Background(),
		`SELECT action FROM request_events WHERE request_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatalf("history of request %d: %v", id, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// auditCount counts audit rows for one action on one request.
func (w *requestWorkWorld) auditCount(t *testing.T, action string, requestID int) int {
	t.Helper()
	var n int
	if err := w.db(t).QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = $1 AND target_type = 'request' AND target_id = $2`,
		action, fmt.Sprint(requestID)).Scan(&n); err != nil {
		t.Fatalf("count %s audit rows: %v", action, err)
	}
	return n
}

// ---- criterion 1a: the view matrix ----

// TestMCPRequestWork_ViewMatrix is the transport-level half of the access matrix:
//
//   - a team member reads the queue, its per-state counts and its rows;
//   - a granted customer — who may submit — is answered not-found, because a
//     submit grant is not a read (contract §2), and the answer names no team;
//   - a stranger from another org gets the same not-found;
//   - a request in a sibling team is not in this team's queue, and reading it by
//     its id through this team is not-found rather than found-and-refused;
//   - the team's read is the record (revision, claim, assignee, org), while the
//     requester's read of their own request by public_ref is the whitelist
//     projection and carries no internal field.
func TestMCPRequestWork_ViewMatrix(t *testing.T) {
	w := newRequestWorkWorld(t)

	t.Run("member reads the queue with its counts", func(t *testing.T) {
		page := w.mustTool(t, w.member, "amp_list_requests", map[string]interface{}{"team_id": float64(w.team)})
		if intField(t, page, "count") < 1 {
			t.Fatalf("queue is empty: %v", page)
		}
		rows, ok := page["items"].([]interface{})
		if !ok || len(rows) == 0 {
			t.Fatalf("queue has no items: %v", page)
		}
		ids := map[int]bool{}
		for _, raw := range rows {
			row, _ := raw.(map[string]interface{})
			ids[intField(t, row, "id")] = true
			if intField(t, row, "team_id") != w.team {
				t.Fatalf("queue row %v belongs to team %v, not %d", row["id"], row["team_id"], w.team)
			}
		}
		if !ids[w.refID] {
			t.Fatalf("the fixture request %d is not in its own team's queue: %v", w.refID, ids)
		}
		// The sibling team's request must not be here: the queue is one team's.
		for id := range ids {
			if id == w.stored(t, w.otherRef).ID {
				t.Fatalf("the sibling team's request %d is in team %d's queue", id, w.team)
			}
		}
		// The counts are the server's own aggregate for the same filter: every
		// queued row falls in exactly one state, and the totals' total is at
		// least this page.
		totals, ok := page["totals"].(map[string]interface{})
		if !ok {
			t.Fatalf("queue has no totals: %v", page)
		}
		if intField(t, totals, "total") < intField(t, page, "count") {
			t.Fatalf("totals.total %v < page count %v", totals["total"], page["count"])
		}
		states, ok := totals["states"].(map[string]interface{})
		if !ok {
			t.Fatalf("totals has no per-state counts: %v", totals)
		}
		sum := 0
		for _, n := range states {
			sum += int(n.(float64))
		}
		if sum != intField(t, totals, "total") {
			t.Fatalf("per-state counts sum to %d, totals.total = %v", sum, totals["total"])
		}
	})

	t.Run("granted requester and stranger are answered not-found", func(t *testing.T) {
		for _, c := range []struct {
			who  *auth.Identity
			name string
		}{
			{w.customer, "the granted customer (a submit grant is not a read)"},
			{w.other, "a customer of another org with no standing here"},
		} {
			got := w.tool(t, c.who, "amp_list_requests", map[string]interface{}{"team_id": float64(w.team)})
			wantWorkRefusal(t, c.name+" reading the queue", got, 404, authz.CodeRequestNotFound)

			got = w.tool(t, c.who, "amp_get_request", map[string]interface{}{
				"team_id": float64(w.team), "request_id": float64(w.refID)})
			wantWorkRefusal(t, c.name+" reading a request by id", got, 404, authz.CodeRequestNotFound)
		}
	})

	t.Run("a sibling team's request is not reachable through this team", func(t *testing.T) {
		otherID := w.stored(t, w.otherRef).ID
		got := w.tool(t, w.member, "amp_get_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(otherID)})
		wantWorkRefusal(t, "a request from another team", got, 404, authz.CodeRequestNotFound)

		// The same bytes as an id that never existed: existence never leaks.
		unknown := w.tool(t, w.member, "amp_get_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(999999999)})
		wantWorkRefusal(t, "an unknown request id", unknown, 404, authz.CodeRequestNotFound)
		if got.errMessage != unknown.errMessage {
			t.Fatalf("another team's request says %q, an unknown id says %q: the two must be indistinguishable",
				got.errMessage, unknown.errMessage)
		}
	})

	t.Run("team view is the record, requester view is the whitelist", func(t *testing.T) {
		teamView := w.mustTool(t, w.member, "amp_get_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(w.refID)})
		if strField(t, teamView, "view") != "team" {
			t.Fatalf("member's view = %q, want team", teamView["view"])
		}
		record, ok := teamView["request"].(map[string]interface{})
		if !ok {
			t.Fatalf("team view carries no request: %v", teamView)
		}
		for _, want := range []string{"id", "rev", "state", "org_id", "team_id", "subject", "public_ref", "claim_user_id", "owner_user_id"} {
			if _, ok := record[want]; !ok {
				t.Errorf("the team's record is missing %q: %v", want, record)
			}
		}

		// A customer reads their own submission by its opaque reference. This
		// fixture's request was filed by the member, so the granted customer
		// submitting one of their own is the requester read.
		own := w.callTool(t, w.customer, mcp.SubmitScopeTool, submitArgs(w.team, "customer's own "+uniq("rw"), ""))
		if own.errMessage != "" {
			t.Fatalf("customer submission refused: %s %v", own.errMessage, own.errData)
		}
		ownRef := requestField(t, own, "public_ref").(string)

		requesterView := w.mustTool(t, w.customer, "amp_get_request", map[string]interface{}{"public_ref": ownRef})
		if strField(t, requesterView, "view") != "requester" {
			t.Fatalf("customer's view = %q, want requester", requesterView["view"])
		}
		projection, ok := requesterView["request"].(map[string]interface{})
		if !ok {
			t.Fatalf("requester view carries no request: %v", requesterView)
		}
		for _, leaked := range []string{
			"id", "org_id", "team_id", "claim_user_id", "owner_user_id", "rev", "metadata",
			"requester_user_id", "requester_contact", "source_external_id", "duplicate_of", "reject_reason",
		} {
			if _, ok := projection[leaked]; ok {
				t.Errorf("the requester's projection leaks %q: %v", leaked, projection)
			}
		}
		for _, want := range []string{"public_ref", "state", "state_word", "subject", "milestones"} {
			if _, ok := projection[want]; !ok {
				t.Errorf("the requester's projection is missing %q: %v", want, projection)
			}
		}
		if strField(t, projection, "public_ref") != ownRef {
			t.Errorf("projection public_ref = %v, want %q", projection["public_ref"], ownRef)
		}

		// And another requester's reference is not-found for them: the
		// own-request read is scoped by the durable requester column.
		notTheirs := w.tool(t, w.other, "amp_get_request", map[string]interface{}{"public_ref": ownRef})
		wantWorkRefusal(t, "another requester reading a request by its reference", notTheirs, 404, authz.CodeRequestNotFound)

		// Naming it both ways is the adapter's/surface's refusal, not a guess.
		both := w.tool(t, w.member, "amp_get_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(w.refID), "public_ref": w.ref})
		wantWorkRefusal(t, "naming a request by id and reference", both, 400, authz.CodeRequestInvalid)
	})

	t.Run("a queue filter narrows the page and its counts", func(t *testing.T) {
		page := w.mustTool(t, w.member, "amp_list_requests", map[string]interface{}{
			"team_id": float64(w.team), "states": []interface{}{"triaging"}})
		if intField(t, page, "count") != 0 {
			t.Fatalf("the triaging queue has rows: %v", page["items"])
		}
		totals, _ := page["totals"].(map[string]interface{})
		if totals == nil || intField(t, totals, "total") != 0 {
			t.Fatalf("the triaging queue's totals = %v, want 0: a filtered pill counts the filter", totals)
		}

		// A state that is not a state is refused with the field named, never an
		// empty queue (a typo matching nothing reads as "no work", which is the
		// one answer a queue must never invent).
		bad := w.tool(t, w.member, "amp_list_requests", map[string]interface{}{
			"team_id": float64(w.team), "states": []interface{}{"pending"}})
		wantWorkRefusal(t, "an unknown state filter", bad, 400, authz.CodeRequestInvalid)
		if !strings.Contains(bad.errMessage, "states") {
			t.Fatalf("the refusal does not name the field: %q", bad.errMessage)
		}

		badSource := w.tool(t, w.member, "amp_list_requests", map[string]interface{}{
			"team_id": float64(w.team), "source_kind": "email"})
		wantWorkRefusal(t, "an unknown source filter", badSource, 400, authz.CodeRequestInvalid)
		if !strings.Contains(badSource.errMessage, "source_kind") {
			t.Fatalf("the refusal does not name the field: %q", badSource.errMessage)
		}

		// The source filter that does exist finds the submission's own source.
		mcpRows := w.mustTool(t, w.member, "amp_list_requests", map[string]interface{}{
			"team_id": float64(w.team), "source_kind": "mcp"})
		if intField(t, mcpRows, "count") < 1 {
			t.Fatalf("the mcp-sourced queue is empty although every fixture submission came through this tool")
		}
	})
}

// ---- criterion 1b: the compare-and-set rules ----

// TestMCPRequestWork_EditCAS: the versioned edit through the tool is a
// compare-and-set, and the write boundary is the service's:
//
//   - the submitter edits their own untriaged request at the revision they read
//     (changed=true, rev bumped, one `edit` history row, one audit row);
//   - the same edit at the revision they no longer have is 409 request.conflict
//     naming the revision the row is actually at — never a lost update;
//   - the same edit with nothing changed is a no-op (changed=false) that writes
//     nothing and bumps nothing;
//   - a team member who did not submit it is refused (403: content edits are the
//     submitter's), while the submitter's own `outcome_brief` is refused as a
//     400 naming the field — the delivery note is a lifecycle act, not an edit.
func TestMCPRequestWork_EditCAS(t *testing.T) {
	w := newRequestWorkWorld(t)

	// A fresh request of the member's own, so the edit window is open and no
	// other subtest's write has moved its revision.
	sub := w.callTool(t, w.member, mcp.SubmitScopeTool, submitArgs(w.team, "edit me "+uniq("rw"), ""))
	if sub.errMessage != "" {
		t.Fatalf("fixture submission refused: %s %v", sub.errMessage, sub.errData)
	}
	ref := requestField(t, sub, "public_ref").(string)
	id := w.stored(t, ref).ID
	rev := w.rev(t, id)
	if rev != 1 {
		t.Fatalf("a new request is at rev %d, want 1", rev)
	}

	t.Run("the submitter's edit applies at the revision they read", func(t *testing.T) {
		out := w.mustTool(t, w.member, "amp_update_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(id), "expected_rev": float64(rev),
			"subject": "edited subject " + ref, "input_brief": "edited brief",
		})
		if changed, _ := out["changed"].(bool); !changed {
			t.Fatalf("changed = %v, want true for a real edit: %v", out["changed"], out)
		}
		stored := w.stored(t, ref)
		if stored.Subject != "edited subject "+ref {
			t.Fatalf("stored subject = %q, want the edited one", stored.Subject)
		}
		if got := w.rev(t, id); got != rev+1 {
			t.Fatalf("rev = %d, want %d (the edit bumps it once)", got, rev+1)
		}
		if n := w.auditCount(t, "request.update", id); n != 1 {
			t.Fatalf("%d request.update audit rows, want 1", n)
		}
		if actions := w.historyActions(t, id); actions[len(actions)-1] != "edit" {
			t.Fatalf("history = %v, want the edit appended last", actions)
		}
	})

	t.Run("a stale revision is 409 naming both revisions", func(t *testing.T) {
		before := w.rev(t, id)
		got := w.tool(t, w.member, "amp_update_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(id), "expected_rev": float64(rev),
			"subject": "should not land",
		})
		wantWorkRefusal(t, "an edit at a stale revision", got, 409, authz.CodeRequestConflict)
		if !strings.Contains(got.errMessage, fmt.Sprint(before)) {
			t.Fatalf("the conflict %q does not name the current rev %d", got.errMessage, before)
		}
		if after := w.rev(t, id); after != before {
			t.Fatalf("rev moved from %d to %d on a refused edit", before, after)
		}
		if stored := w.stored(t, ref); stored.Subject == "should not land" {
			t.Fatal("a refused edit wrote its subject")
		}
	})

	t.Run("an edit that changes nothing writes nothing", func(t *testing.T) {
		before := w.rev(t, id)
		out := w.mustTool(t, w.member, "amp_update_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(id), "expected_rev": float64(before),
			"subject": "edited subject " + ref,
		})
		if changed, _ := out["changed"].(bool); changed {
			t.Fatalf("changed = true for an edit that changed nothing: %v", out)
		}
		if got := w.rev(t, id); got != before {
			t.Fatalf("rev moved from %d to %d on a no-op edit", before, got)
		}
	})

	t.Run("someone who did not submit it cannot rewrite it", func(t *testing.T) {
		before := w.rev(t, id)
		got := w.tool(t, w.admin, "amp_update_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(id), "expected_rev": float64(before),
			"subject": "the org owner rewrites it",
		})
		wantWorkRefusal(t, "a non-submitter's edit", got, 403, "")
		if !strings.Contains(got.errMessage, "submitted it") {
			t.Fatalf("the refusal does not say content is the submitter's: %q", got.errMessage)
		}
	})

	t.Run("the delivery note is not an edit's field", func(t *testing.T) {
		got := w.tool(t, w.member, "amp_update_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(id), "expected_rev": float64(w.rev(t, id)),
			"outcome_brief": "we delivered it",
		})
		wantWorkRefusal(t, "an edit carrying outcome_brief", got, 400, authz.CodeRequestInvalid)
		if !strings.Contains(got.errMessage, "outcome_brief") {
			t.Fatalf("the refusal does not name the field: %q", got.errMessage)
		}
	})

	t.Run("metadata is not an edit's field", func(t *testing.T) {
		got := w.tool(t, w.member, "amp_update_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(id), "expected_rev": float64(w.rev(t, id)),
			"metadata": map[string]interface{}{"severity": "page"},
		})
		wantWorkRefusal(t, "an edit carrying metadata", got, 400, authz.CodeRequestInvalid)
		if !strings.Contains(got.errMessage, "metadata") {
			t.Fatalf("the refusal does not name the field: %q", got.errMessage)
		}
	})

	t.Run("a wrong-typed revision is refused before the write", func(t *testing.T) {
		before := w.rev(t, id)
		for _, bad := range []interface{}{"2", true, map[string]interface{}{}, 1.5} {
			got := w.tool(t, w.member, "amp_update_request", map[string]interface{}{
				"team_id": float64(w.team), "request_id": float64(id), "expected_rev": bad,
				"subject": "typed rev " + fmt.Sprint(bad),
			})
			wantWorkRefusal(t, fmt.Sprintf("a %T revision", bad), got, 400, authz.CodeRequestInvalid)
			if !strings.Contains(got.errMessage, "expected_rev") {
				t.Fatalf("the refusal does not name expected_rev: %q", got.errMessage)
			}
		}
		if after := w.rev(t, id); after != before {
			t.Fatalf("a refused edit moved rev from %d to %d", before, after)
		}
	})

	t.Run("a non-submitter cannot edit a triaged request either", func(t *testing.T) {
		// The member claims first, which is what makes the request triaged; the
		// submitter's own edit is then refused for that reason, named as such.
		w.mustTool(t, w.member, "amp_claim_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(id)})
		got := w.tool(t, w.member, "amp_update_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(id), "expected_rev": float64(w.rev(t, id)),
			"subject": "an edit after triage began",
		})
		wantWorkRefusal(t, "the submitter's edit after triage began", got, 403, "")
		if !strings.Contains(got.errMessage, "picked up") {
			t.Fatalf("the refusal does not say the request was picked up: %q", got.errMessage)
		}
	})
}

// TestMCPRequestWork_ClaimRaceAndReplay: one claim wins, the other is 409 naming
// the holder, and a replay writes nothing.
//
// The race here is sequential rather than concurrent on purpose: the concurrent
// half is the repository's own `-race` suite (request_claims_test.go), and what
// this test must prove is that the *tool* does not lose the service's answer —
// the holder's id and the 409 code survive the transport.
func TestMCPRequestWork_ClaimRaceAndReplay(t *testing.T) {
	w := newRequestWorkWorld(t)

	sub := w.callTool(t, w.member, mcp.SubmitScopeTool, submitArgs(w.team, "claim me "+uniq("rw"), ""))
	if sub.errMessage != "" {
		t.Fatalf("fixture submission refused: %s %v", sub.errMessage, sub.errData)
	}
	id := w.stored(t, requestField(t, sub, "public_ref").(string)).ID

	// The member takes it. An org owner is also a triager here (InboxRole's
	// allow-set), so the second claimant is a real second caller.
	first := w.mustTool(t, w.member, "amp_claim_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id)})
	if changed, _ := first["changed"].(bool); !changed {
		t.Fatalf("the first claim did not write: %v", first)
	}
	claim, _ := w.claimOf(t, id)
	if claim == nil || *claim != w.member.User.ID {
		t.Fatalf("stored claim = %v, want %d", claim, w.member.User.ID)
	}

	second := w.tool(t, w.admin, "amp_claim_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id)})
	wantWorkRefusal(t, "a claim on a request someone else holds", second, 409, authz.CodeRequestConflict)
	if !strings.Contains(second.errMessage, fmt.Sprint(w.member.User.ID)) {
		t.Fatalf("the conflict %q does not name the holder %d", second.errMessage, w.member.User.ID)
	}
	claim, _ = w.claimOf(t, id)
	if claim == nil || *claim != w.member.User.ID {
		t.Fatalf("the lost race stole the claim: holder is now %v", claim)
	}

	// A replay by the holder is a no-op: no rev bump, no second history row, no
	// second audit row.
	rev, history, audits := w.rev(t, id), len(w.historyActions(t, id)), w.auditCount(t, "request.claim", id)
	again := w.mustTool(t, w.member, "amp_claim_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id)})
	if changed, _ := again["changed"].(bool); changed {
		t.Fatalf("a replay of the same claim wrote: %v", again)
	}
	if got := w.rev(t, id); got != rev {
		t.Fatalf("a replayed claim bumped rev from %d to %d", rev, got)
	}
	if got := len(w.historyActions(t, id)); got != history {
		t.Fatalf("a replayed claim appended a history row: %d -> %d", history, got)
	}
	if got := w.auditCount(t, "request.claim", id); got != audits {
		t.Fatalf("a replayed claim wrote another audit row: %d -> %d", audits, got)
	}

	// A requester has no claim to take on their own request.
	own := w.callTool(t, w.customer, mcp.SubmitScopeTool, submitArgs(w.team, "customer claim "+uniq("rw"), ""))
	if own.errMessage != "" {
		t.Fatalf("customer submission refused: %s %v", own.errMessage, own.errData)
	}
	ownID := w.stored(t, requestField(t, own, "public_ref").(string)).ID
	// A requester can see this request (it is theirs), so the refusal is the
	// honest 403 explaining why a claim is a triager's act — a 404 would be a lie
	// about a row the caller is already reading.
	customerClaim := w.tool(t, w.customer, "amp_claim_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(ownID)})
	wantWorkRefusal(t, "a requester claiming their own request", customerClaim, 403, "")
	if !strings.Contains(customerClaim.errMessage, "triager") {
		t.Fatalf("the refusal does not say claiming is a triager's act: %q", customerClaim.errMessage)
	}
	claim, _ = w.claimOf(t, ownID)
	if claim != nil {
		t.Fatalf("a requester's call wrote a claim: %v", claim)
	}

	// A team member with no standing on this team's inbox learns nothing.
	otherTeamClaim := w.tool(t, w.member, "amp_claim_request", map[string]interface{}{
		"team_id": float64(w.foreign), "request_id": float64(id)})
	wantWorkRefusal(t, "a claim aimed at another team", otherTeamClaim, 404, authz.CodeRequestNotFound)
}

// TestMCPRequestWork_ReleaseAndAssign: release is the holder's act and is
// idempotent; assignment is a manager's explicit transfer that clears the claim
// in the same write; and neither can move the request's lifecycle state.
func TestMCPRequestWork_ReleaseAndAssign(t *testing.T) {
	w := newRequestWorkWorld(t)

	sub := w.callTool(t, w.member, mcp.SubmitScopeTool, submitArgs(w.team, "assign me "+uniq("rw"), ""))
	if sub.errMessage != "" {
		t.Fatalf("fixture submission refused: %s %v", sub.errMessage, sub.errData)
	}
	id := w.stored(t, requestField(t, sub, "public_ref").(string)).ID

	// The member claims, then releases. A second release is a no-op.
	w.mustTool(t, w.member, "amp_claim_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id)})
	release := w.mustTool(t, w.member, "amp_release_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id)})
	if changed, _ := release["changed"].(bool); !changed {
		t.Fatalf("the release did not write: %v", release)
	}
	if claim, _ := w.claimOf(t, id); claim != nil {
		t.Fatalf("stored claim = %v after a release, want none", claim)
	}
	rev := w.rev(t, id)
	again := w.mustTool(t, w.member, "amp_release_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id)})
	if changed, _ := again["changed"].(bool); changed {
		t.Fatalf("a repeated release wrote: %v", again)
	}
	if got := w.rev(t, id); got != rev {
		t.Fatalf("a repeated release bumped rev from %d to %d", rev, got)
	}

	// Someone else's claim cannot be released: it is the mirror of a steal.
	w.mustTool(t, w.member, "amp_claim_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id)})
	foreignRelease := w.tool(t, w.admin, "amp_release_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id)})
	wantWorkRefusal(t, "releasing someone else's claim", foreignRelease, 409, authz.CodeRequestConflict)
	if claim, _ := w.claimOf(t, id); claim == nil || *claim != w.member.User.ID {
		t.Fatalf("the failed release changed the claim: %v", claim)
	}

	// A plain member cannot assign (that is a manager's gate), and the refusal is
	// the manager's own words.
	memberAssign := w.tool(t, w.member, "amp_assign_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id), "assignee": "me"})
	wantWorkRefusal(t, "a plain member assigning a request", memberAssign, 403, "")
	if !strings.Contains(memberAssign.errMessage, "team lead or org admin") {
		t.Fatalf("the refusal does not name the manager gate: %q", memberAssign.errMessage)
	}

	// The org owner may assign, and the transfer clears the member's claim in the
	// same write (D4/#3654 C5: no instant with a new owner beside a stale claim).
	assign := w.mustTool(t, w.admin, "amp_assign_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id), "assignee": w.admin.User.Email})
	if changed, _ := assign["changed"].(bool); !changed {
		t.Fatalf("the assignment did not write: %v", assign)
	}
	claim, owner := w.claimOf(t, id)
	if claim != nil {
		t.Fatalf("the transfer left claim %v in place", claim)
	}
	if owner == nil || *owner != w.admin.User.ID {
		t.Fatalf("stored owner = %v, want %d", owner, w.admin.User.ID)
	}
	if n := w.auditCount(t, "request.assign", id); n != 1 {
		t.Fatalf("%d request.assign audit rows, want 1", n)
	}

	// Assigning what is already set is a no-op.
	rev = w.rev(t, id)
	repeat := w.mustTool(t, w.admin, "amp_assign_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id), "assignee": w.admin.User.Email})
	if changed, _ := repeat["changed"].(bool); changed {
		t.Fatalf("a repeated assignment wrote: %v", repeat)
	}
	if got := w.rev(t, id); got != rev {
		t.Fatalf("a repeated assignment bumped rev from %d to %d", rev, got)
	}

	// Unassigning: "none" leaves the request unowned, which is a state a request
	// may legitimately be put back into.
	w.mustTool(t, w.admin, "amp_assign_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id), "assignee": "none"})
	if _, owner := w.claimOf(t, id); owner != nil {
		t.Fatalf("stored owner = %v after unassigning, want none", owner)
	}

	// A user who cannot see the inbox cannot be handed the request: they would
	// own work they cannot open.
	customerAssign := w.tool(t, w.admin, "amp_assign_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id), "assignee": w.customer.User.Email})
	wantWorkRefusal(t, "assigning to a user with no standing on the inbox", customerAssign, 400, authz.CodeRequestInvalid)

	// And nothing in this whole flow moved the request's state: claims,
	// releases and assignments are triage bookkeeping, not the lifecycle.
	if state := w.stateOf(t, id); state != string(domain.RequestStatusNew) {
		t.Fatalf("state = %q after claims/releases/assignments, want new: only the transition function moves a request", state)
	}
}

// TestMCPRequestWork_Discussion: the comment tool speaks in the caller's own
// voice and attributes the row to the authenticated caller — and a retry naming
// the row it already wrote writes nothing.
func TestMCPRequestWork_Discussion(t *testing.T) {
	w := newRequestWorkWorld(t)

	sub := w.callTool(t, w.member, mcp.SubmitScopeTool, submitArgs(w.team, "discuss me "+uniq("rw"), ""))
	if sub.errMessage != "" {
		t.Fatalf("fixture submission refused: %s %v", sub.errMessage, sub.errData)
	}
	id := w.stored(t, requestField(t, sub, "public_ref").(string)).ID

	// A triager's comment is attributed to them, and writes one row.
	out := w.mustTool(t, w.member, "amp_comment_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id), "body": "looking into this now"})
	if wrote, _ := out["wrote"].(bool); !wrote {
		t.Fatalf("wrote = false for a first comment: %v", out)
	}
	comment, ok := out["comment"].(map[string]interface{})
	if !ok {
		t.Fatalf("answer carries no comment: %v", out)
	}
	if author := strField(t, comment, "author"); author != w.member.User.Email && author != w.member.Name {
		t.Fatalf("comment author = %q, want the authenticated caller", author)
	}
	if intField(t, comment, "author_user_id") != w.member.User.ID {
		t.Fatalf("comment author_user_id = %v, want %d", comment["author_user_id"], w.member.User.ID)
	}
	commentID := intField(t, comment, "id")

	// A retry naming that row answers with it and writes nothing.
	rev := w.rev(t, id)
	retry := w.mustTool(t, w.member, "amp_comment_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id), "body": "looking into this now",
		"retry_of_comment_id": float64(commentID)})
	if wrote, _ := retry["wrote"].(bool); wrote {
		t.Fatalf("a retried comment wrote a second row: %v", retry)
	}
	retried, _ := retry["comment"].(map[string]interface{})
	if intField(t, retried, "id") != commentID {
		t.Fatalf("the retry answered comment %v, want %d", retried["id"], commentID)
	}
	var rows int
	if err := w.db(t).QueryRow(context.Background(),
		`SELECT count(*) FROM request_comments WHERE request_id = $1`, id).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d comment rows after one comment and its retry, want 1", rows)
	}
	if got := w.rev(t, id); got != rev {
		t.Fatalf("a comment bumped rev from %d to %d: a comment changes nothing but itself", rev, got)
	}

	// The voices are the service's: a triager cannot answer on the requester's
	// behalf, and a requester cannot ask themselves a question.
	triagerAnswer := w.tool(t, w.member, "amp_comment_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id), "body": "answered on their behalf", "kind": "answer"})
	wantWorkRefusal(t, "a triager posting an answer", triagerAnswer, 403, "")

	// The requester's own request: a `comment` is allowed (it is their voice),
	// and an `answer` is theirs too, without moving the state (answering the
	// question and leaving needs_information is the requester's web-UI act).
	own := w.callTool(t, w.customer, mcp.SubmitScopeTool, submitArgs(w.team, "customer thread "+uniq("rw"), ""))
	if own.errMessage != "" {
		t.Fatalf("customer submission refused: %s %v", own.errMessage, own.errData)
	}
	ownID := w.stored(t, requestField(t, own, "public_ref").(string)).ID
	requesterComment := w.mustTool(t, w.customer, "amp_comment_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(ownID), "body": "a note from the requester"})
	if wrote, _ := requesterComment["wrote"].(bool); !wrote {
		t.Fatalf("the requester's own comment was not written: %v", requesterComment)
	}
	requesterQuestion := w.tool(t, w.customer, "amp_comment_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(ownID), "body": "why?", "kind": "question"})
	wantWorkRefusal(t, "a requester posting a question", requesterQuestion, 403, "")

	// The system's own voice is never a caller's.
	system := w.tool(t, w.member, "amp_comment_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id), "body": "system note", "kind": "system"})
	wantWorkRefusal(t, "a caller posting a system comment", system, 403, "")

	// An unknown kind is request.invalid, not a silent plain comment.
	unknownKind := w.tool(t, w.member, "amp_comment_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id), "body": "kind", "kind": "nudge"})
	wantWorkRefusal(t, "an unknown comment kind", unknownKind, 400, authz.CodeRequestInvalid)

	// A blank body is refused by the domain, named as the field.
	blank := w.tool(t, w.member, "amp_comment_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(id), "body": "   "})
	wantWorkRefusal(t, "a blank comment body", blank, 400, authz.CodeRequestInvalid)
	if !strings.Contains(blank.errMessage, "body") {
		t.Fatalf("the refusal does not name body: %q", blank.errMessage)
	}
}

// ---- criterion 2: schema and catalog, on the wire ----

// TestMCPRequestWork_SchemaAndCatalog reads tools/list off the real server, so
// what is asserted is what an MCP client receives: the seven tools are listed
// with titles and truthful annotations, and the two reads are readOnly while the
// four writes are not.
func TestMCPRequestWork_SchemaAndCatalog(t *testing.T) {
	w := newRequestWorkWorld(t)

	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	b, err := json.Marshal(w.s.srv.HandleMessage(w.s.callCtx(), raw))
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Title       string `json:"title"`
				Description string `json:"description"`
				InputSchema struct {
					Properties map[string]struct {
						Description string `json:"description"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"inputSchema"`
				Annotations struct {
					ReadOnly    *bool `json:"readOnlyHint"`
					Idempotent  *bool `json:"idempotentHint"`
					Destructive *bool `json:"destructiveHint"`
				} `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("decode tools/list: %v (%s)", err, b)
	}
	byName := map[string]int{}
	for i, tl := range list.Result.Tools {
		byName[tl.Name] = i
	}
	reads := map[string]bool{"amp_list_requests": true, "amp_get_request": true}
	for _, name := range requestWorkToolNames {
		i, ok := byName[name]
		if !ok {
			t.Errorf("%s is not listed for a team member", name)
			continue
		}
		tl := list.Result.Tools[i]
		if tl.Title == "" {
			t.Errorf("%s has no title", name)
		}
		if tl.Annotations.ReadOnly == nil || *tl.Annotations.ReadOnly != reads[name] {
			t.Errorf("%s readOnlyHint = %v, want %v", name, tl.Annotations.ReadOnly, reads[name])
		}
		if tl.Annotations.Idempotent == nil || *tl.Annotations.Idempotent != (reads[name] || !reads[name]) {
			// every tool here is idempotent (reads trivially, writes by the
			// service's replay rules), so this only fails if the hint went nil.
			t.Errorf("%s idempotentHint = %v, want true", name, tl.Annotations.Idempotent)
		}
		if tl.Annotations.Destructive == nil || *tl.Annotations.Destructive {
			t.Errorf("%s destructiveHint = %v, want false", name, tl.Annotations.Destructive)
		}
		for prop, schema := range tl.InputSchema.Properties {
			if strings.TrimSpace(schema.Description) == "" {
				t.Errorf("%s.%s has no description on the wire", name, prop)
			}
		}
		// No argument names the actor, an acceptance or a plan target: the
		// schema is where a model learns what it may send.
		for _, forbidden := range []string{"user_id", "actor", "accept", "delivered", "project_id", "task_id", "state", "author"} {
			if _, ok := tl.InputSchema.Properties[forbidden]; ok {
				t.Errorf("%s.%s is on the wire; this slice's tools cannot set it", name, forbidden)
			}
		}
	}

	// The required sets a client is told to send.
	want := map[string][]string{
		"amp_list_requests":   {"team_id"},
		"amp_get_request":     nil,
		"amp_update_request":  {"expected_rev", "request_id", "team_id"},
		"amp_comment_request": {"body", "request_id", "team_id"},
		"amp_claim_request":   {"request_id", "team_id"},
		"amp_release_request": {"request_id", "team_id"},
		"amp_assign_request":  {"assignee", "request_id", "team_id"},
	}
	for name, wantReq := range want {
		i, ok := byName[name]
		if !ok {
			continue
		}
		got := list.Result.Tools[i].InputSchema.Required
		if len(got) != len(wantReq) {
			t.Errorf("%s required = %v, want %v", name, got, wantReq)
			continue
		}
		for j := range wantReq {
			if got[j] != wantReq[j] {
				t.Errorf("%s required = %v, want %v", name, got, wantReq)
			}
		}
	}
}

// ---- criterion 3: the boundaries ----

// TestMCPRequestWork_SubmitOnlyCredentialIsRefused is AMP #3772's boundary from
// this slice's side, through the real server: a submit-only credential may call
// amp_submit_request and nothing else, and its tools/list advertises exactly
// that one tool — so none of the seven read/triage tools is reachable, however
// the credential is pointed.
func TestMCPRequestWork_SubmitOnlyCredentialIsRefused(t *testing.T) {
	w := newRequestWorkWorld(t)
	scoped := w.scoped(t, w.member, w.team)

	// tools/list: exactly the one tool the scope allows, none of the seven.
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	stack := w.s.as(scoped)
	resp := stack.srv.HandleMessage(stack.callCtx(), raw)
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("decode tools/list: %v (%s)", err, b)
	}
	var names []string
	for _, tl := range list.Result.Tools {
		names = append(names, tl.Name)
	}
	if len(names) != 1 || names[0] != mcp.SubmitScopeTool {
		t.Fatalf("scoped tools/list = %v, want exactly [%s]", names, mcp.SubmitScopeTool)
	}

	// And every one of the seven is refused when called anyway.
	//
	// The refusal comes from submitScopeGate (token_scope.go), the server-wide
	// middleware, before any handler runs — which is why it is the same answer
	// every other tool gives a bound credential, and why it arrives without a
	// request.* code in the error's data (the code is authz.CodeTokenScopeDenied;
	// request_submit_mcp_test.go asserts that at the HTTP layer for the same
	// gate). What matters here is that the refusal is the scope's and not a 404
	// that would confirm a team, so the message is what this asserts.
	for _, name := range requestWorkToolNames {
		got := w.tool(t, scoped, name, map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(w.refID), "expected_rev": float64(1),
			"assignee": "me", "body": "x", "public_ref": w.ref,
		})
		if got.errMessage == "" {
			t.Fatalf("%s was allowed for a submit-only credential: %v", name, got.result)
		}
		if !strings.Contains(got.errMessage, "may only submit") {
			t.Fatalf("%s refusal = %q, want the scope refusal", name, got.errMessage)
		}
	}

	// A full-scope token is unaffected: the same person's token reads the queue.
	full := w.fullToken(w.member)
	if page := w.mustTool(t, full, "amp_list_requests", map[string]interface{}{"team_id": float64(w.team)}); intField(t, page, "count") < 1 {
		t.Fatalf("a full-scope token's queue is empty: %v", page)
	}
}

// TestMCPRequestWork_NoAcceptanceOrLifecycleTool is the negative boundary as a
// property of the surface: there is no tool through which a credential can
// accept a delivery, deliver one, or move a state — and after every act above,
// the request's state is still what the submitter left it as.
//
// The list of forbidden verbs is deliberately the ones that stay forbidden: the
// triage slice (#3969) may not attach or plan, and the planning slice (#3972)
// may — so "attach", "link" and "handoff" left this list when those tools
// landed, while acceptance, delivery and every lifecycle verb stayed. What the
// two tests now share is the guarantee that outlives both slices: no request
// tool can accept or deliver anything, because those are the requester's and the
// team's own acts in the web UI (I1, D5).
func TestMCPRequestWork_NoAcceptanceOrLifecycleTool(t *testing.T) {
	w := newRequestWorkWorld(t)

	// No tool name on the surface offers those acts. A tool that existed would be
	// reachable the moment it was registered, so its absence is the boundary.
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	b, err := json.Marshal(w.s.srv.HandleMessage(w.s.callCtx(), raw))
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	for _, tl := range list.Result.Tools {
		// The ticket surface's own tools (amp_complete_task, amp_block_task, …)
		// are about tickets, not requests; the check is scoped to the request
		// tools so it cannot fail on a name that merely contains "accept".
		if !strings.Contains(tl.Name, "request") {
			continue
		}
		for _, forbidden := range []string{"accept", "deliver", "transition", "reopen", "answer", "reject"} {
			if strings.Contains(tl.Name, forbidden) {
				t.Fatalf("the request surface has %q, which belongs to the requester's own web-UI act or to the lifecycle (I1, D5)", tl.Name)
			}
		}
	}

	// Calling an acceptance-shaped name is "tool not found", not a refusal that
	// would imply the capability exists but is denied.
	got := w.tool(t, w.member, "amp_accept_request", map[string]interface{}{"request_id": float64(w.refID)})
	if !strings.Contains(got.errMessage, "not found") {
		t.Fatalf("amp_accept_request answered %q; there is no such tool", got.errMessage)
	}

	// And the submitter's request is still `new` after the whole slice's surface
	// has been exercised against it: no tool here can advance a lifecycle.
	if state := w.stateOf(t, w.refID); state != string(domain.RequestStatusNew) {
		t.Fatalf("state = %q, want new: this slice's tools change content, claims and assignees, never the lifecycle", state)
	}

	// No attachment or link row was written for THESE requests by anything in this
	// slice. The assertion is scoped to this world's requests rather than to the
	// whole table: the suite shares one database, and later slices legitimately
	// write both tables (the planning and attachment tools, #3972), so a
	// count-of-zero over the table would assert "this slice does not exist" — the
	// same trap request_links_test.go's assertNothingLinked documents. "Wrote no
	// more" is the property, and it is checked for both of this fixture's
	// requests.
	for _, requestID := range []int{w.refID, w.stored(t, w.otherRef).ID} {
		var attachments, links int
		if err := w.db(t).QueryRow(context.Background(),
			`SELECT (SELECT count(*) FROM request_attachments WHERE request_id = $1),
			        (SELECT count(*) FROM request_links WHERE request_id = $1)`, requestID).Scan(&attachments, &links); err != nil {
			t.Fatal(err)
		}
		if attachments != 0 || links != 0 {
			t.Fatalf("request %d has %d attachments and %d links: this slice has no tool that creates either", requestID, attachments, links)
		}
	}
}
