package main

// Verification receipts, Phase 2 (#126, v2/docs/specs/verification-receipts.md):
// the receipt model, checks, the derived status, the Off / Inform / Require
// setting, the hold, and a person's Accept / Reject, end to end through the
// real MCP tools and REST routes.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// setReceiptsMode sets the project's mode as its owner in the web UI.
func setReceiptsMode(t *testing.T, ra *restAs, projectID int, mode string) {
	t.Helper()
	ra.want(t, 200, "owner", http.MethodPatch, fmt.Sprintf("/api/projects/%d", projectID), map[string]any{"receipts_mode": mode})
}

// receiptRows counts a ticket's receipt rows, drafts included.
func receiptRows(t *testing.T, taskID int) int {
	t.Helper()
	ctx := context.Background()
	db, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	var n int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM task_receipts WHERE task_id = $1`, taskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func getReceipt(t *testing.T, ra *restAs, who string, taskID int) map[string]any {
	t.Helper()
	return ra.want(t, 200, who, http.MethodGet, fmt.Sprintf("/api/tasks/%d/receipt", taskID), nil)
}

func receiptStatus(t *testing.T, ra *restAs, taskID int) string {
	t.Helper()
	s, _ := getReceipt(t, ra, "alice", taskID)["status"].(string)
	return s
}

func taskState(t *testing.T, s *mcpStack, id int) domain.TaskState {
	t.Helper()
	got, err := s.repo.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got.State
}

func passCheck(ref string) map[string]any {
	return map[string]any{"criterion_ref": ref, "kind": "test", "verdict": "pass", "command": "go test ./...", "exit_code": float64(0)}
}

// Off (the default): nothing is recorded and completion is unchanged.
func TestReceipts_OffByDefault(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	id := mkIn(t, alice, w.s.storyID, "off ticket")

	proj := ra.want(t, 200, "owner", http.MethodGet, fmt.Sprintf("/api/projects/%d", w.s.projectID), nil)["project"].(map[string]any)
	if proj["receipts_mode"] != "" || proj["receipts_min_level"] != "self_checked" {
		t.Fatalf("a new project's setting: %v / %v", proj["receipts_mode"], proj["receipts_min_level"])
	}
	org := ra.want(t, 200, "orgadmin", http.MethodGet, fmt.Sprintf("/api/orgs/%d/receipts", w.org.ID), nil)
	if org["mode"] != "off" {
		t.Fatalf("org default: %v", org)
	}

	dispatch := dispatchClaims(t, alice, id)
	open := alice.call(t, "amp_open_receipt", map[string]any{"task_id": float64(id), "dispatch_id": dispatch})
	if intField(t, open, "receipt_id") != 0 || open["mode"] != "off" {
		t.Fatalf("open in an Off project: %v", open)
	}
	out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(id), "dispatch_id": dispatch,
		"checks": []any{passCheck("1")}, "branch": "feat/x", "commit": "abcdef1234"})
	if out["state"] != "completed" || out["held"] != nil || out["receipt_status"] != nil {
		t.Fatalf("complete: %v", out)
	}
	if n := receiptRows(t, id); n != 0 {
		t.Fatalf("Off recorded %d receipts", n)
	}
	got := getReceipt(t, ra, "bob", id)
	if got["receipt"] != nil || got["mode"] != "off" {
		t.Fatalf("GET receipt in Off: %v", got)
	}
	sum := ra.want(t, 200, "bob", http.MethodGet, fmt.Sprintf("/api/projects/%d/receipts/summary", w.s.projectID), nil)
	if sum["mode"] != "off" || len(sum["tasks"].([]any)) != 0 {
		t.Fatalf("summary: %v", sum)
	}
}

// Inform: receipts are recorded and shown; nothing waits.
func TestReceipts_Inform(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	setReceiptsMode(t, ra, w.s.projectID, "inform")

	// Checks give self_checked; the run's git details are on the receipt.
	a := mkIn(t, alice, w.s.storyID, "checked")
	da := dispatchClaims(t, alice, a)
	open := alice.call(t, "amp_open_receipt", map[string]any{"task_id": float64(a), "dispatch_id": da, "summary": "did it",
		"files_changed": []any{map[string]any{"path": "a.go", "added": float64(3), "removed": float64(1)}}})
	rid := intField(t, open, "receipt_id")
	if rid == 0 || open["mode"] != "inform" {
		t.Fatalf("open: %v", open)
	}
	// A second open for the same dispatch is the same draft.
	if again := alice.call(t, "amp_open_receipt", map[string]any{"task_id": float64(a), "dispatch_id": da}); intField(t, again, "receipt_id") != rid {
		t.Fatalf("reopen: %v, want %d", again, rid)
	}
	// Only the ticket's current dispatch opens one.
	wantErr(t, alice.callExpectError(t, "amp_open_receipt", map[string]any{"task_id": float64(a), "dispatch_id": "stale"}), "current dispatch")
	out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(a), "dispatch_id": da, "receipt_id": float64(rid),
		"checks": []any{passCheck("1"), map[string]any{"criterion_ref": "2", "kind": "visual", "verdict": "cant_tell", "note": "no screenshot"}},
		"branch": "feat/a", "commit": "abc1234def", "base_branch": "main", "tree_sha": "tree123"})
	if out["state"] != "completed" || out["receipt_status"] != "self_checked" {
		t.Fatalf("complete: %v", out)
	}
	got := getReceipt(t, ra, "bob", a)
	rc := got["receipt"].(map[string]any)
	if got["status"] != "self_checked" || got["held"] != false || intField(t, rc, "id") != rid || rc["state"] != "final" ||
		rc["branch"] != "feat/a" || rc["commit_sha"] != "abc1234def" || rc["base_branch"] != "main" || rc["tree_sha"] != "tree123" ||
		rc["summary"] != "did it" || rc["mode"] != "inform" || rc["run_id"] == nil {
		t.Fatalf("receipt: %v", got)
	}
	if cs := got["checks"].([]any); len(cs) != 2 || cs[0].(map[string]any)["role"] != "worker" {
		t.Fatalf("checks: %v", cs)
	}
	// Completing twice leaves one receipt.
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(a), "dispatch_id": da, "checks": []any{passCheck("1")}})
	if n := receiptRows(t, a); n != 1 {
		t.Fatalf("after a repeat completion: %d receipts", n)
	}

	// No checks give unverified; an old plugin's call is unchanged.
	b := mkIn(t, alice, w.s.storyID, "unchecked")
	db := dispatchClaims(t, alice, b)
	if out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(b), "dispatch_id": db}); out["state"] != "completed" {
		t.Fatalf("old call: %v", out)
	}
	if st := receiptStatus(t, ra, b); st != "unverified" {
		t.Fatalf("no checks: %s", st)
	}

	// A UI completion adopts the worker's draft.
	c := mkIn(t, alice, w.s.storyID, "ui done")
	dc := dispatchClaims(t, alice, c)
	draft := intField(t, alice.call(t, "amp_open_receipt", map[string]any{"task_id": float64(c), "dispatch_id": dc}), "receipt_id")
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", c), nil)
	if rc := getReceipt(t, ra, "alice", c)["receipt"].(map[string]any); intField(t, rc, "id") != draft || rc["state"] != "final" {
		t.Fatalf("UI completion receipt: %v, want draft %d", rc, draft)
	}
	if n := receiptRows(t, c); n != 1 {
		t.Fatalf("UI completion: %d receipts", n)
	}

	// Another ticket's receipt_id is refused, and the ticket stays open.
	d := mkIn(t, alice, w.s.storyID, "refused")
	e := mkIn(t, alice, w.s.storyID, "other")
	dd, de := dispatchClaims(t, alice, d), dispatchClaims(t, alice, e)
	other := intField(t, alice.call(t, "amp_open_receipt", map[string]any{"task_id": float64(e), "dispatch_id": de}), "receipt_id")
	wantErr(t, alice.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(d), "dispatch_id": dd, "receipt_id": float64(other)}),
		fmt.Sprintf("receipt %d isn't ticket %d's", other, d))
	if st := taskState(t, alice, d); st != domain.TaskStateInProgress {
		t.Fatalf("refused completion left the ticket %s", st)
	}
	// A malformed check refuses too.
	wantErr(t, alice.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(d), "dispatch_id": dd,
		"checks": []any{map[string]any{"kind": "test"}}}), "verdict is required")

	// The summary lists each ticket's latest status.
	sum := ra.want(t, 200, "bob", http.MethodGet, fmt.Sprintf("/api/projects/%d/receipts/summary", w.s.projectID), nil)
	byTask := map[int]string{}
	for _, row := range sum["tasks"].([]any) {
		m := row.(map[string]any)
		byTask[intField(t, m, "task_id")] = m["status"].(string)
	}
	if sum["mode"] != "inform" || byTask[a] != "self_checked" || byTask[b] != "unverified" || byTask[c] != "unverified" {
		t.Fatalf("summary: %v", sum)
	}
}

// Require: a completion below the minimum is held for a person.
func TestReceipts_Require(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	setReceiptsMode(t, ra, w.s.projectID, "require")

	// Below the minimum: held, still in progress, the comment posted, the
	// dependent waiting, the dispatch over.
	x := mkIn(t, alice, w.s.storyID, "held")
	y := mkIn(t, alice, w.s.storyID, "after held", x)
	dx := dispatchClaims(t, alice, x)
	out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(x), "dispatch_id": dx,
		"checks": []any{map[string]any{"criterion_ref": "1", "kind": "visual", "verdict": "cant_tell", "note": "evidence missing"}},
		"branch": "feat/x", "pr_url": "https://github.com/acme/app/pull/7"})
	if out["held"] != true || out["state"] != "in_progress" || !strings.Contains(out["reason"].(string), "couldn't tell") {
		t.Fatalf("held completion: %v", out)
	}
	if st := taskState(t, alice, x); st != domain.TaskStateInProgress {
		t.Fatalf("held ticket is %s", st)
	}
	if cur, _ := alice.repo.GetTask(context.Background(), x); cur.DispatchID != "" {
		t.Fatalf("held ticket still holds dispatch %q", cur.DispatchID)
	}
	if c := lastComment(t, alice, x); !strings.Contains(c, "Couldn't verify, needs you") {
		t.Fatalf("held comment: %q", c)
	}
	if st := taskState(t, alice, y); st != domain.TaskStateBlocked {
		t.Fatalf("dependent of a held ticket is %s", st)
	}
	got := getReceipt(t, ra, "alice", x)
	if got["held"] != true || got["status"] != "unverified" || got["receipt"].(map[string]any)["run_id"] == nil {
		t.Fatalf("held receipt: %v", got)
	}
	sum := ra.want(t, 200, "bob", http.MethodGet, fmt.Sprintf("/api/projects/%d/receipts/summary", w.s.projectID), nil)
	held := false
	for _, row := range sum["tasks"].([]any) {
		if m := row.(map[string]any); intField(t, m, "task_id") == x && m["held"] == true {
			held = true
		}
	}
	if !held {
		t.Fatalf("summary doesn't show the hold: %v", sum)
	}

	// Accept completes it as human_accepted, keeps it, and the dependent starts.
	acc := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/receipt/accept", x), map[string]any{"note": "looked at it"})
	if acc["status"] != "human_accepted" || acc["held"] != false || acc["receipt"].(map[string]any)["kept"] != true {
		t.Fatalf("accept: %v", acc)
	}
	if st := taskState(t, alice, x); st != domain.TaskStateCompleted {
		t.Fatalf("accepted ticket is %s", st)
	}
	// Its PR still waits on a merge, as a normal completion's would.
	if cur, _ := alice.repo.GetTask(context.Background(), x); cur.MergeState != domain.MergeNeeded {
		t.Fatalf("accepted ticket's merge state: %q", cur.MergeState)
	}
	if n := receiptRows(t, x); n != 1 {
		t.Fatalf("accept made %d receipts", n)
	}

	// Reject (with a note) returns it to ready.
	z := mkIn(t, alice, w.s.storyID, "rejected")
	dz := dispatchClaims(t, alice, z)
	if out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(z), "dispatch_id": dz}); out["held"] != true {
		t.Fatalf("no checks under Require: %v", out)
	}
	ra.want(t, 400, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/receipt/reject", z), map[string]any{})
	rej := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/receipt/reject", z), map[string]any{"note": "the button is missing"})
	if rej["status"] != "unverified" || rej["held"] != false {
		t.Fatalf("reject: %v", rej)
	}
	if st := taskState(t, alice, z); st != domain.TaskStateBacklog {
		t.Fatalf("rejected ticket is %s", st)
	}
	if c := lastComment(t, alice, z); !strings.Contains(c, "the button is missing") {
		t.Fatalf("reject comment: %q", c)
	}

	// Meeting the minimum completes normally.
	ok := mkIn(t, alice, w.s.storyID, "meets")
	dok := dispatchClaims(t, alice, ok)
	if out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(ok), "dispatch_id": dok,
		"checks": []any{passCheck("1")}}); out["state"] != "completed" || out["held"] != nil {
		t.Fatalf("meets the minimum: %v", out)
	}

	// A person's Done below the minimum is held the same way, with Accept right there.
	ui := mkIn(t, alice, w.s.storyID, "ui done")
	dispatchClaims(t, alice, ui)
	done := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", ui), nil)
	if done["held"] != true {
		t.Fatalf("UI Done under Require: %v", done)
	}
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/receipt/accept", ui), nil)
	if st := taskState(t, alice, ui); st != domain.TaskStateCompleted {
		t.Fatalf("accepted UI ticket is %s", st)
	}

	// amp_set_task_state(completed) is held too.
	ss := mkIn(t, alice, w.s.storyID, "set state")
	dss := dispatchClaims(t, alice, ss)
	if out := alice.call(t, "amp_set_task_state", map[string]any{"task_id": float64(ss), "state": "completed", "dispatch_id": dss}); out["held"] != true {
		t.Fatalf("set state completed under Require: %v", out)
	}
	if st := taskState(t, alice, ss); st != domain.TaskStateInProgress {
		t.Fatalf("set state completed under Require left it %s", st)
	}
	if got := getReceipt(t, ra, "alice", ss); got["held"] != true {
		t.Fatalf("set state receipt: %v", got)
	}
}

// Only a person in the web UI changes the setting: never an agent, and
// never a lead. An agent may make a ticket's override stricter, not weaker.
func TestReceipts_SettingAndOverrides(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	pid := w.s.projectID
	path := fmt.Sprintf("/api/projects/%d", pid)

	for _, ch := range []string{auth.ChannelToken, auth.ChannelMCP} {
		if code, out := ra.doVia(t, ch, "owner", http.MethodPatch, path, map[string]any{"receipts_mode": "inform"}); code != 403 {
			t.Fatalf("agent change via %s: %d %v", ch, code, out)
		}
	}
	ra.want(t, 403, "lead", http.MethodPatch, path, map[string]any{"receipts_mode": "inform"})
	ra.want(t, 400, "owner", http.MethodPatch, path, map[string]any{"receipts_mode": "sometimes"})
	setReceiptsMode(t, ra, pid, "inform")
	entries, err := w.s.repo.ListAudit(context.Background(), domain.AuditFilter{ProjectID: pid, Action: "project.settings"})
	if err != nil || len(entries) == 0 {
		t.Fatalf("audit: %v %v", entries, err)
	}
	if e := entries[0]; e.Before["receipts_mode"] != "" || e.After["receipts_mode"] != "inform" {
		t.Fatalf("audit before/after: %v / %v", e.Before, e.After)
	}

	// The org default: org admins, in the web UI only.
	opath := fmt.Sprintf("/api/orgs/%d/receipts", w.org.ID)
	ra.want(t, 403, "owner", http.MethodPut, opath, map[string]any{"mode": "inform"})
	if code, _ := ra.doVia(t, auth.ChannelToken, "orgadmin", http.MethodPut, opath, map[string]any{"mode": "inform"}); code != 403 {
		t.Fatalf("org default via a token: %d", code)
	}
	if got := ra.want(t, 200, "orgadmin", http.MethodPut, opath, map[string]any{"mode": "require"}); got["mode"] != "require" {
		t.Fatalf("org default: %v", got)
	}
	// The project's own value still wins; '' then follows the org.
	other := mkIn(t, alice, w.s.storyID, "org default")
	if s := getReceipt(t, ra, "alice", other)["settings"].(map[string]any); s["mode"] != "inform" || s["org_mode"] != "require" {
		t.Fatalf("settings with a project value: %v", s)
	}
	setReceiptsMode(t, ra, pid, "")
	if s := getReceipt(t, ra, "alice", other)["settings"].(map[string]any); s["mode"] != "require" {
		t.Fatalf("settings using the org's: %v", s)
	}
	setReceiptsMode(t, ra, pid, "inform")

	// A ticket override: stricter is fine from an agent, weaker isn't.
	id := mkIn(t, alice, w.s.storyID, "override")
	alice.call(t, "amp_update_task", map[string]any{"task_id": float64(id), "metadata": map[string]any{"receipts": "require"}})
	wantErr(t, alice.callExpectError(t, "amp_update_task", map[string]any{"task_id": float64(id), "metadata": map[string]any{"receipts": "inform"}}),
		"web UI")
	wantErr(t, alice.callExpectError(t, "amp_update_task", map[string]any{"task_id": float64(id), "metadata": map[string]any{"receipts": nil}}),
		"web UI")
	wantErr(t, alice.callExpectError(t, "amp_update_task", map[string]any{"task_id": float64(id), "metadata": map[string]any{"receipts": "maybe"}}),
		"off, inform or require")
	if s := getReceipt(t, ra, "alice", id)["settings"].(map[string]any); s["mode"] != "require" {
		t.Fatalf("override: %v", s)
	}
	// An epic override is guarded the same way, and so is a new ticket.
	owner := w.as(w.owner)
	wantErr(t, owner.callExpectError(t, "amp_update_epic", map[string]any{"epic_id": float64(w.s.epicID), "metadata": map[string]any{"receipts": "off"}}),
		"web UI")
	owner.call(t, "amp_update_epic", map[string]any{"epic_id": float64(w.s.epicID), "metadata": map[string]any{"receipts": "require"}})
	wantErr(t, alice.callExpectError(t, "amp_create_task", map[string]any{"project_id": float64(pid), "story_id": float64(w.s.storyID),
		"name": "weak", "description": "d", "acceptance_criteria": "a", "metadata": map[string]any{"receipts": "off"}}), "web UI")
	// A person in the web UI may lower it.
	ra.want(t, 200, "alice", http.MethodPatch, fmt.Sprintf("/api/tasks/%d", id), map[string]any{"metadata": map[string]any{"receipts": "off"}})
	if s := getReceipt(t, ra, "alice", id)["settings"].(map[string]any); s["mode"] != "off" {
		t.Fatalf("UI lowered override: %v", s)
	}
}

// Accept and Reject are a person's, in the web UI; Accept and a merge keep
// the receipt.
func TestReceipts_AcceptRulesAndKeep(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	setReceiptsMode(t, ra, w.s.projectID, "inform")

	a := mkIn(t, alice, w.s.storyID, "accept me")
	da := dispatchClaims(t, alice, a)
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(a), "dispatch_id": da, "checks": []any{passCheck("1")}})
	accept := fmt.Sprintf("/api/tasks/%d/receipt/accept", a)
	for _, ch := range []string{auth.ChannelToken, auth.ChannelMCP} {
		if code, out := ra.doVia(t, ch, "alice", http.MethodPost, accept, map[string]any{}); code != 403 {
			t.Fatalf("agent Accept via %s: %d %v", ch, code, out)
		}
	}
	// Someone who can't work the ticket can't either.
	if code, _ := ra.do(t, "carol", http.MethodPost, accept, map[string]any{}); code != 403 {
		t.Fatalf("carol accepted alice's ticket: %d", code)
	}
	ra.want(t, 400, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/receipt/reject", a), map[string]any{"note": "  "})
	got := ra.want(t, 200, "alice", http.MethodPost, accept, map[string]any{})
	if got["status"] != "human_accepted" || got["receipt"].(map[string]any)["kept"] != true {
		t.Fatalf("accept: %v", got)
	}
	entries, _ := w.s.repo.ListAudit(context.Background(), domain.AuditFilter{ProjectID: w.s.projectID, Action: "receipt.accept"})
	if len(entries) != 1 {
		t.Fatalf("receipt.accept audit: %v", entries)
	}
	// A later Reject overrides the Accept: the status falls back to the
	// worker's checks (the drawer shows the rejection). The ticket stays
	// done: it wasn't held.
	rej := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/receipt/reject", a), map[string]any{"note": "wrong colour"})
	if rej["status"] != "self_checked" || taskState(t, alice, a) != domain.TaskStateCompleted {
		t.Fatalf("reject after accept: %v", rej)
	}

	// A merge keeps the ticket's receipts.
	b := mkIn(t, alice, w.s.storyID, "merge me")
	db := dispatchClaims(t, alice, b)
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(b), "dispatch_id": db, "checks": []any{passCheck("1")},
		"branch": "feat/b", "pr_url": "https://github.com/acme/app/pull/9"})
	if rc := getReceipt(t, ra, "alice", b)["receipt"].(map[string]any); rc["kept"] != false {
		t.Fatalf("kept before merge: %v", rc)
	}
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/merge", b), nil)
	if rc := getReceipt(t, ra, "alice", b)["receipt"].(map[string]any); rc["kept"] != true {
		t.Fatalf("kept after merge: %v", rc)
	}

	// Reopening and completing again makes a new receipt; the older one is an attempt.
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/reopen", a), nil)
	da2 := dispatchClaims(t, alice, a)
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(a), "dispatch_id": da2})
	if got := getReceipt(t, ra, "alice", a); got["status"] != "unverified" || intField(t, got, "attempts") != 1 {
		t.Fatalf("second attempt: %v", got)
	}
	if list := ra.want(t, 200, "bob", http.MethodGet, fmt.Sprintf("/api/tasks/%d/receipts", a), nil)["receipts"].([]any); len(list) != 2 {
		t.Fatalf("receipts list: %v", list)
	}
}

// A held ticket moved some other way (back to ready, then dispatched again)
// isn't held any more: no "needs you", and Accept doesn't complete the new
// attempt.
func TestReceipts_StaleHoldIsCleared(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	setReceiptsMode(t, ra, w.s.projectID, "require")
	x := mkIn(t, alice, w.s.storyID, "moved while held")
	dx := dispatchClaims(t, alice, x)
	if out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(x), "dispatch_id": dx}); out["held"] != true {
		t.Fatalf("not held: %v", out)
	}
	// A "change" to the state it already has keeps the hold.
	alice.call(t, "amp_set_task_state", map[string]any{"task_id": float64(x), "state": "in_progress"})
	if got := getReceipt(t, ra, "alice", x); got["held"] != true {
		t.Fatalf("a no-op state change cleared the hold: %v", got)
	}
	alice.call(t, "amp_set_task_state", map[string]any{"task_id": float64(x), "state": "backlog"})
	if got := getReceipt(t, ra, "alice", x); got["held"] != false {
		t.Fatalf("held after a move to ready: %v", got)
	}
	dispatchClaims(t, alice, x)
	if got := getReceipt(t, ra, "alice", x); got["held"] != false {
		t.Fatalf("held after the new dispatch: %v", got)
	}
	acc := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/receipt/accept", x), nil)
	if acc["held"] != false || taskState(t, alice, x) != domain.TaskStateInProgress {
		t.Fatalf("Accept on a stale hold: %v, ticket %s", acc, taskState(t, alice, x))
	}
	sum := ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/receipts/summary", w.s.projectID), nil)
	for _, row := range sum["tasks"].([]any) {
		if m := row.(map[string]any); intField(t, m, "task_id") == x && m["held"] != false {
			t.Fatalf("summary still holds it: %v", m)
		}
	}
}

// A held completion is an ask in "waiting on you": its owner's, or with no
// owner, the project owners'.
func TestReceipts_HeldIsWaitingOnYou(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	setReceiptsMode(t, ra, w.s.projectID, "require")
	x := mkIn(t, alice, w.s.storyID, "held ask")
	dx := dispatchClaims(t, alice, x)
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(x), "dispatch_id": dx})

	verifyItems := func(who string) []any {
		t.Helper()
		got := ra.want(t, 200, who, http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work", w.s.projectID), nil)
		items, _ := got["items"].(map[string]any)["verify"].([]any)
		if n := intField(t, got["counts"].(map[string]any), "verify"); n != len(items) {
			t.Fatalf("%s: verify count %d, items %v", who, n, items)
		}
		return items
	}
	items := verifyItems("alice")
	if len(items) != 1 || intField(t, items[0].(map[string]any), "task_id") != x ||
		!strings.Contains(items[0].(map[string]any)["hold_reason"].(string), "no checks") {
		t.Fatalf("alice's verify items: %v", items)
	}
	if len(verifyItems("owner")) != 0 {
		t.Fatal("an owned held ticket shows on a project owner's list")
	}
	counts := ra.want(t, 200, "alice", http.MethodGet, "/api/my-work/counts", nil)
	if intField(t, counts["counts"].(map[string]any), "verify") != 1 {
		t.Fatalf("my-work counts: %v", counts)
	}

	// Nobody's: each project owner's.
	ctx := context.Background()
	db, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	if _, err := db.Exec(ctx, `UPDATE tasks SET owner_user_id = NULL WHERE id = $1`, x); err != nil {
		t.Fatal(err)
	}
	if len(verifyItems("owner")) != 1 || len(verifyItems("alice")) != 0 || len(verifyItems("bob")) != 0 {
		t.Fatal("an unowned held ticket should be on the project owners' lists only")
	}

	// Accepted, it's off everyone's list.
	ra.want(t, 200, "owner", http.MethodPost, fmt.Sprintf("/api/tasks/%d/receipt/accept", x), nil)
	if len(verifyItems("owner")) != 0 {
		t.Fatal("still waiting after Accept")
	}
}

// An agent can't weaken a ticket's requirement by moving it, or its story,
// out from under a stricter override.
func TestReceipts_MovesAreGuarded(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	owner := w.as(w.owner)
	setReceiptsMode(t, ra, w.s.projectID, "inform")
	strictEpic, strictStory := newStory(t, owner, "strict")
	_, looseStory := newStory(t, owner, "loose")
	looseEpic := intField(t, owner.call(t, "amp_create_epic", map[string]any{"project_id": float64(w.s.projectID), "name": "loose 2"}), "id")
	owner.call(t, "amp_update_epic", map[string]any{"epic_id": float64(strictEpic), "metadata": map[string]any{"receipts": "require"}})

	id := mkIn(t, owner, strictStory, "strict ticket")
	wantErr(t, owner.callExpectError(t, "amp_move_task", map[string]any{"task_id": float64(id), "story_id": float64(looseStory)}),
		"web UI", "require")
	wantErr(t, owner.callExpectError(t, "amp_move_story", map[string]any{"story_id": float64(strictStory), "epic_id": float64(looseEpic)}),
		"web UI")
	// Toward a stricter (or equal) setting is fine.
	other := mkIn(t, owner, looseStory, "loose ticket")
	owner.call(t, "amp_move_task", map[string]any{"task_id": float64(other), "story_id": float64(strictStory)})
	// A ticket with its own require override moves anywhere.
	own := mkIn(t, owner, strictStory, "own override")
	owner.call(t, "amp_update_task", map[string]any{"task_id": float64(own), "metadata": map[string]any{"receipts": "require"}})
	owner.call(t, "amp_move_task", map[string]any{"task_id": float64(own), "story_id": float64(looseStory)})
	// A person in the web UI may move it.
	ra.want(t, 200, "owner", http.MethodPost, fmt.Sprintf("/api/tasks/%d/move", id), map[string]any{"story_id": looseStory})
}

// The move guards fail closed: what they can't read refuses the move
// (retryable) rather than judging it against nothing inherited.
func TestReceipts_MoveGuardsFailClosed(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	agent := authz.Principal{UserID: w.owner.User.ID, Email: w.owner.User.Email, Channel: auth.ChannelMCP}
	story, err := w.s.repo.GetStory(ctx, w.s.storyID)
	if err != nil {
		t.Fatal(err)
	}
	missing := &domain.Epic{ID: 1 << 30, ProjectID: w.s.projectID}
	if err := svc.GuardReceiptsMoveStory(ctx, agent, story, missing); authz.StatusOf(err, 0) != 503 ||
		!strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("story move with unreadable target: %v", err)
	}
	ghost := &domain.Task{ID: 1 << 30, ProjectID: w.s.projectID}
	if err := svc.GuardReceiptsMoveTask(ctx, agent, ghost, story); authz.StatusOf(err, 0) != 503 {
		t.Fatalf("task move with unreadable ticket: %v", err)
	}
}
