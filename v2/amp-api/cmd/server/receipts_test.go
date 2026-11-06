package main

// Verification receipts, Phases 2 and 3 (#126,
// v2/docs/specs/verification-receipts.md): the receipt model, checks, the
// derived status, the Off / Inform / Require setting, the hold, a person's
// Accept / Reject, and the evidence store (uploads, serving, the visual-check
// downgrade, retention, purge), end to end through the real MCP tools and
// REST routes.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/receipts"
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
		"branch": "feat/a", "commit": "abc1234def", "base_branch": "main", "tree_sha": "abcdef0123456789"})
	if out["state"] != "completed" || out["receipt_status"] != "self_checked" {
		t.Fatalf("complete: %v", out)
	}
	got := getReceipt(t, ra, "bob", a)
	rc := got["receipt"].(map[string]any)
	if got["status"] != "self_checked" || got["held"] != false || intField(t, rc, "id") != rid || rc["state"] != "final" ||
		rc["branch"] != "feat/a" || rc["commit_sha"] != "abc1234def" || rc["base_branch"] != "main" || rc["tree_sha"] != "abcdef0123456789" ||
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
	// A person moving it in the web UI (an agent can't, see
	// TestReceipts_HeldTicketCantBeMovedOffUI): a "change" to the state it
	// already has keeps the hold, a move to ready ends it.
	uiSetState(t, w, w.alice, x, domain.TaskStateInProgress)
	if got := getReceipt(t, ra, "alice", x); got["held"] != true {
		t.Fatalf("a no-op state change cleared the hold: %v", got)
	}
	uiSetState(t, w, w.alice, x, domain.TaskStateBacklog)
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

// ---- Phase 3: the evidence store, serving, retention ----

// evidenceWorld is an Inform-or-Require world with a receipt store, on both
// the MCP tools and the REST routes: local, in a temporary directory, or S3
// when RECEIPT_S3_TEST_ENDPOINT is set (see testEvidenceStore).
type evidenceWorld struct {
	w     *rbacWorld
	ra    *restAs
	ev    *service.EvidenceStore
	store receipts.Store
}

// s3TestBucket is one bucket per test process, so keys (project and task
// ids) from an earlier run against the same store never collide.
var s3TestBucket = fmt.Sprintf("amp-cmd-test-%d", time.Now().UnixNano())

// testEvidenceStore is the store an evidence test runs on: local in a
// temporary directory, or, with RECEIPT_S3_TEST_ENDPOINT (and _ACCESS_KEY,
// _SECRET_KEY; _USE_SSL defaults to false), S3 in a bucket of its own.
func testEvidenceStore(t *testing.T) receipts.Store {
	t.Helper()
	if ep := os.Getenv("RECEIPT_S3_TEST_ENDPOINT"); ep != "" {
		s3, err := receipts.NewS3(receipts.S3Config{
			Endpoint: ep, Bucket: s3TestBucket, PathStyle: true, SpoolDir: t.TempDir(),
			AccessKey: os.Getenv("RECEIPT_S3_TEST_ACCESS_KEY"), SecretKey: os.Getenv("RECEIPT_S3_TEST_SECRET_KEY"),
			UseSSL: os.Getenv("RECEIPT_S3_TEST_USE_SSL") == "true",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s3.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s3
	}
	local, err := receipts.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return local
}

func newEvidenceWorld(t *testing.T, mode string) *evidenceWorld {
	t.Helper()
	w := newRBACWorld(t)
	store := testEvidenceStore(t)
	ev := &service.EvidenceStore{Store: store, MaxBytes: 1 << 20, Retention: 90 * 24 * time.Hour}
	w.s.tools.SetReceiptStore(ev)
	t.Cleanup(func() { w.s.tools.SetReceiptStore(nil) })
	ra := newRestAsWith(t, w, func(h *api.RestHandler) { h.SetReceiptStore(ev) })
	setReceiptsMode(t, ra, w.s.projectID, mode)
	return &evidenceWorld{w: w, ra: ra, ev: ev, store: store}
}

// svc is a service on the world's store, as main's background jobs get.
func (e *evidenceWorld) svc() *service.Service {
	s := service.New(e.w.s.repo, e.w.s.reg, nil)
	s.Evidence = e.ev
	return s
}

// inStore reports whether the store holds key.
func (e *evidenceWorld) inStore(key string) bool {
	err := e.store.Serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil), key)
	return err == nil
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// put uploads body to an upload URL (a bare path: APP_PUBLIC_URL is unset).
func (e *evidenceWorld) put(t *testing.T, url string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, e.ra.server.URL+url, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// get fetches evidence as who, with an optional Range.
func (e *evidenceWorld) get(t *testing.T, who, key, rng string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.ra.server.URL+"/api/receipts/"+key, nil)
	req.Header.Set("X-Test-As", who)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func evidenceStatus(t *testing.T, key string) string {
	t.Helper()
	ctx := context.Background()
	db, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	var st string
	if err := db.QueryRow(ctx, `SELECT status FROM receipt_evidence WHERE key = $1`, key).Scan(&st); err != nil {
		return "none"
	}
	return st
}

type upload struct{ name, key, url string }

func uploadsOf(t *testing.T, out map[string]any) map[string]upload {
	t.Helper()
	got := map[string]upload{}
	for _, u := range out["uploads"].([]any) {
		m := u.(map[string]any)
		url, _ := m["url"].(string)
		got[m["name"].(string)] = upload{name: m["name"].(string), key: m["key"].(string), url: url}
	}
	return got
}

func file(name string, body []byte, extra ...any) map[string]any {
	m := map[string]any{"name": name, "bytes": float64(len(body)), "sha256": sha256hex(body)}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

const treeA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// opened dispatches a new ticket and opens its receipt.
func opened(t *testing.T, s *mcpStack, storyID int, name string) (taskID int, dispatch string, receiptID int) {
	t.Helper()
	taskID = mkIn(t, s, storyID, name)
	dispatch = dispatchClaims(t, s, taskID)
	// A ticket a test leaves dispatched and unstarted would be returned by
	// a later run's watchdog test (it sweeps every project): mark it started.
	t.Cleanup(func() { execSQL(t, `UPDATE tasks SET started_at = COALESCE(started_at, NOW()) WHERE id = $1`, taskID) })
	receiptID = intField(t, s.call(t, "amp_open_receipt", map[string]any{"task_id": float64(taskID), "dispatch_id": dispatch}), "receipt_id")
	if receiptID == 0 {
		t.Fatal("open receipt returned 0")
	}
	return
}

// Upload, then a Range GET plays. A sha mismatch is 422 and stores nothing;
// a reused token is 404, an expired one 410; a non-member's GET is 403.
// Retries work; another dispatch is refused. Evidence never lists as an
// artifact.
func TestReceipts_EvidenceUploadAndServe(t *testing.T) {
	e := newEvidenceWorld(t, "inform")
	alice := e.w.as(e.w.alice)
	a, da, ra := opened(t, alice, e.w.s.storyID, "with evidence")
	png := []byte("\x89PNG\r\n\x1a\n fake screenshot bytes")
	mp4 := []byte("....ftypisom fake video bytes, long enough to seek through")

	add := func(dispatch string, files ...any) map[string]any {
		return alice.call(t, "amp_add_evidence", map[string]any{"receipt_id": float64(ra), "dispatch_id": dispatch,
			"commit": "abc1234", "tree_sha": treeA, "files": files})
	}
	addErr := func(dispatch string, files ...any) string {
		return alice.callExpectError(t, "amp_add_evidence", map[string]any{"receipt_id": float64(ra), "dispatch_id": dispatch,
			"tree_sha": treeA, "files": files})
	}
	// Another dispatch_id is refused; so are names off the allowlist, and
	// files over RECEIPT_MAX_BYTES.
	wantErr(t, addErr("stale-dispatch", file("screenshot-1.png", png)), "dispatch_id")
	wantErr(t, addErr(da, file("index.html", png)), "isn't an evidence name")
	wantErr(t, addErr(da, file("image.svg", png)), "isn't an evidence name")
	wantErr(t, addErr(da, map[string]any{"name": "video.mp4", "bytes": float64(2 << 20), "sha256": sha256hex(mp4)}), "RECEIPT_MAX_BYTES")
	wantErr(t, alice.callExpectError(t, "amp_add_evidence", map[string]any{"receipt_id": float64(ra), "dispatch_id": da,
		"files": []any{file("screenshot-1.png", png)}}), "tree_sha is required")

	ups := uploadsOf(t, add(da, file("screenshot-1.png", png), file("video.mp4", mp4, "frames", float64(90), "duration_ms", float64(3000))))
	shot, video := ups["screenshot-1.png"], ups["video.mp4"]
	wantKey := fmt.Sprintf("%d/%d/attempts/%d/%s/screenshot-1.png", e.w.s.projectID, a, ra, treeA[:12])
	if shot.key != wantKey || !strings.HasPrefix(shot.url, "/api/receipt-uploads/") {
		t.Fatalf("upload: %+v, want key %s", shot, wantKey)
	}
	// The receipt records the revision.
	if rc, _ := e.w.s.repo.GetReceipt(context.Background(), int64(ra)); rc.TreeSHA != treeA || rc.CommitSHA != "abc1234" {
		t.Fatalf("receipt revision: %q %q", rc.TreeSHA, rc.CommitSHA)
	}

	// A sha256 mismatch (same length) is 422 and stores nothing.
	bad := bytes.Repeat([]byte("x"), len(png))
	if code, out := e.put(t, shot.url, bad); code != 422 {
		t.Fatalf("mismatched upload: %d, want 422 (%v)", code, out)
	}
	if e.inStore(shot.key) || evidenceStatus(t, shot.key) != "pending" {
		t.Fatalf("a mismatched upload stored something: in the store %v, %s", e.inStore(shot.key), evidenceStatus(t, shot.key))
	}
	// So is a longer body.
	if code, _ := e.put(t, shot.url, append(append([]byte{}, png...), 'x')); code != 422 || e.inStore(shot.key) {
		t.Fatalf("long upload: %d", code)
	}
	// The same URL then works (the retry), once.
	if code, out := e.put(t, shot.url, png); code != 200 || out["status"] != "stored" {
		t.Fatalf("upload: %d %v", code, out)
	}
	if !e.inStore(shot.key) || evidenceStatus(t, shot.key) != "stored" {
		t.Fatal("upload not stored")
	}
	if code, _ := e.put(t, shot.url, png); code != 404 {
		t.Fatalf("reused token: %d, want 404", code)
	}
	if code, _ := e.put(t, "/api/receipt-uploads/not-a-token", png); code != 404 {
		t.Fatalf("junk token: %d, want 404", code)
	}

	// Re-adding: a stored file with the same sha256 has nothing to upload;
	// a different sha256 is refused; a pending one gets a new URL, and the
	// old one stops working.
	again := uploadsOf(t, add(da, file("screenshot-1.png", png), file("video.mp4", mp4, "frames", float64(90))))
	if again["screenshot-1.png"].url != "" || again["video.mp4"].url == "" || again["video.mp4"].url == video.url {
		t.Fatalf("re-add: %+v", again)
	}
	wantErr(t, addErr(da, file("screenshot-1.png", bad)), "different sha256")
	if code, _ := e.put(t, video.url, mp4); code != 404 {
		t.Fatalf("a re-minted row's old URL: %d, want 404", code)
	}
	if code, _ := e.put(t, again["video.mp4"].url, mp4); code != 200 {
		t.Fatalf("re-minted upload: %d", code)
	}

	// An expired URL is 410.
	logs := []byte("console output")
	cons := uploadsOf(t, add(da, file("console.txt", logs)))["console.txt"]
	execSQL(t, `UPDATE receipt_evidence SET upload_expires_at = NOW() - INTERVAL '1 minute' WHERE key = $1`, cons.key)
	if code, _ := e.put(t, cons.url, logs); code != 410 {
		t.Fatalf("expired token: %d, want 410", code)
	}

	// A Range GET plays (206), under the safety headers.
	resp, body := e.get(t, "bob", video.key, "bytes=4-15")
	if resp.StatusCode != http.StatusPartialContent || string(body) != string(mp4[4:16]) ||
		resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 4-15/%d", len(mp4)) {
		t.Fatalf("range GET: %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Range"))
	}
	resp, body = e.get(t, "bob", shot.key, "")
	h := resp.Header
	if resp.StatusCode != 200 || string(body) != string(png) || h.Get("Content-Type") != "image/png" ||
		h.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(h.Get("Content-Security-Policy"), "sandbox") ||
		h.Get("Cross-Origin-Resource-Policy") != "same-origin" || h.Get("Referrer-Policy") != "no-referrer" ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "inline") || h.Get("ETag") != `"`+sha256hex(png)+`"` {
		t.Fatalf("GET: %d %v", resp.StatusCode, h)
	}
	// A non-member is 403, whatever the key; a bad key 400; an unknown one 404.
	if resp, _ := e.get(t, "outsider", shot.key, ""); resp.StatusCode != 403 {
		t.Fatalf("non-member GET: %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.get(t, "outsider", fmt.Sprintf("%d/nope/x.png", e.w.s.projectID), ""); resp.StatusCode != 403 {
		t.Fatalf("non-member GET of an unknown key: %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.get(t, "bob", fmt.Sprintf("%d/a%%5Cb", e.w.s.projectID), ""); resp.StatusCode != 400 {
		t.Fatalf("bad key: %d, want 400", resp.StatusCode)
	}
	if resp, _ := e.get(t, "bob", fmt.Sprintf("%d/1/attempts/1/abc/video.mp4", e.w.s.projectID), ""); resp.StatusCode != 404 {
		t.Fatalf("unknown key: %d, want 404", resp.StatusCode)
	}
	// Not yet uploaded is 404.
	if resp, _ := e.get(t, "bob", cons.key, ""); resp.StatusCode != 404 {
		t.Fatalf("pending GET: %d, want 404", resp.StatusCode)
	}

	// Evidence never lists as an artifact.
	arts := e.ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/artifacts", e.w.s.projectID), nil)
	if n := intField(t, arts, "count"); n != 0 {
		t.Fatalf("evidence listed as %d artifacts: %v", n, arts)
	}

	// Completing with visual checks that cite the stored files counts them;
	// the receipt lists its evidence with when it expires.
	out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(a), "dispatch_id": da, "receipt_id": float64(ra),
		"checks": []any{
			map[string]any{"criterion_ref": "1", "kind": "visual", "verdict": "pass", "evidence_key": shot.key},
			map[string]any{"criterion_ref": "2", "kind": "visual", "verdict": "pass", "evidence_key": video.key},
		}})
	if out["state"] != "completed" || out["receipt_status"] != "self_checked" {
		t.Fatalf("complete: %v", out)
	}
	got := getReceipt(t, e.ra, "bob", a)
	for _, c := range got["checks"].([]any) {
		if c.(map[string]any)["verdict"] != "pass" {
			t.Fatalf("a backed visual check was downgraded: %v", c)
		}
	}
	evs := got["evidence"].([]any)
	if len(evs) != 3 || evs[0].(map[string]any)["status"] != "stored" || evs[0].(map[string]any)["expires_at"] == nil {
		t.Fatalf("evidence: %v", evs)
	}
	// Finalized: no more evidence.
	wantErr(t, addErr(da, file("screenshot-2.png", png)), "")
}

// Missing or foreign evidence downgrades a visual check, as does a
// tree_sha mismatch (but not a missing tree_sha: see
// domain.TestDowngradeVisualChecks).
func TestReceipts_VisualDowngrade(t *testing.T) {
	e := newEvidenceWorld(t, "require")
	alice := e.w.as(e.w.alice)
	png := []byte("\x89PNG screenshot")

	// b has stored evidence; a cites it (foreign) and a missing key.
	b, db, rb := opened(t, alice, e.w.s.storyID, "other receipt")
	bShot := uploadsOf(t, alice.call(t, "amp_add_evidence", map[string]any{"receipt_id": float64(rb), "dispatch_id": db,
		"tree_sha": treeA, "files": []any{file("screenshot-1.png", png)}}))["screenshot-1.png"]
	if code, _ := e.put(t, bShot.url, png); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	a, da, ra := opened(t, alice, e.w.s.storyID, "foreign evidence")
	out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(a), "dispatch_id": da, "receipt_id": float64(ra),
		"checks": []any{
			map[string]any{"criterion_ref": "1", "kind": "visual", "verdict": "pass", "evidence_key": bShot.key},
			map[string]any{"criterion_ref": "2", "kind": "visual", "verdict": "pass", "evidence_key": "nope"},
			map[string]any{"criterion_ref": "3", "kind": "visual", "verdict": "pass"},
		}})
	if out["held"] != true {
		t.Fatalf("visual checks without their own evidence weren't held under Require: %v", out)
	}
	for _, c := range getReceipt(t, e.ra, "alice", a)["checks"].([]any) {
		m := c.(map[string]any)
		if m["verdict"] != "cant_tell" || !strings.HasPrefix(m["note"].(string), "evidence missing") {
			t.Fatalf("check not downgraded: %v", m)
		}
	}

	// A tree_sha mismatch: evidence on tree A, completed on tree B.
	shot := uploadsOf(t, alice.call(t, "amp_add_evidence", map[string]any{"receipt_id": float64(rb), "dispatch_id": db,
		"tree_sha": treeA, "files": []any{file("screenshot-1.png", png)}}))["screenshot-1.png"]
	if shot.url != "" {
		t.Fatalf("stored file re-added: %+v", shot)
	}
	out = alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(b), "dispatch_id": db, "receipt_id": float64(rb),
		"tree_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"checks":   []any{map[string]any{"criterion_ref": "1", "kind": "visual", "verdict": "pass", "evidence_key": bShot.key}}})
	if out["held"] != true {
		t.Fatalf("evidence from another revision wasn't held: %v", out)
	}
	c := getReceipt(t, e.ra, "alice", b)["checks"].([]any)[0].(map[string]any)
	if c["verdict"] != "cant_tell" || c["note"] != "evidence recorded on another revision" {
		t.Fatalf("tree mismatch: %v", c)
	}

	// The same evidence on its own revision counts.
	g, dg, rg := opened(t, alice, e.w.s.storyID, "good evidence")
	gShot := uploadsOf(t, alice.call(t, "amp_add_evidence", map[string]any{"receipt_id": float64(rg), "dispatch_id": dg,
		"tree_sha": treeA, "files": []any{file("screenshot-1.png", png)}}))["screenshot-1.png"]
	if code, _ := e.put(t, gShot.url, png); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	out = alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(g), "dispatch_id": dg, "receipt_id": float64(rg),
		"tree_sha": treeA, "checks": []any{map[string]any{"criterion_ref": "1", "kind": "visual", "verdict": "pass", "evidence_key": gShot.key}}})
	if out["state"] != "completed" || out["held"] != nil {
		t.Fatalf("backed visual check under Require: %v", out)
	}
}

// With no store, Require holds a ticket whose only check is visual, and
// evidence calls say why.
func TestReceipts_NoStoreHoldsVisual(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	setReceiptsMode(t, ra, w.s.projectID, "require")
	x, dx, rx := opened(t, alice, w.s.storyID, "no store")
	wantErr(t, alice.callExpectError(t, "amp_add_evidence", map[string]any{"receipt_id": float64(rx), "dispatch_id": dx,
		"tree_sha": treeA, "files": []any{file("screenshot-1.png", []byte("png"))}}), "evidence storage isn't configured")
	out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(x), "dispatch_id": dx, "receipt_id": float64(rx),
		"checks": []any{map[string]any{"criterion_ref": "1", "kind": "visual", "verdict": "pass", "evidence_key": "anything"}}})
	if out["held"] != true || taskState(t, alice, x) != domain.TaskStateInProgress {
		t.Fatalf("visual-only completion with no store: %v", out)
	}
	c := getReceipt(t, ra, "alice", x)["checks"].([]any)[0].(map[string]any)
	if c["verdict"] != "cant_tell" || !strings.Contains(c["note"].(string), "evidence storage isn't configured") {
		t.Fatalf("check: %v", c)
	}
	// Serving says so too.
	if code, _ := ra.do(t, "alice", http.MethodGet, fmt.Sprintf("/api/receipts/%d/%d/attempts/%d/x/video.mp4", w.s.projectID, x, rx), nil); code != 404 {
		t.Fatalf("GET with no store: %d", code)
	}
}

// The sweep expires unkept evidence and leaves kept evidence; purging a
// ticket from the trash deletes its receipts and evidence; a reset deletes
// the project's.
func TestReceipts_SweepPurgeReset(t *testing.T) {
	e := newEvidenceWorld(t, "inform")
	alice := e.w.as(e.w.alice)
	ctx := context.Background()
	png := []byte("\x89PNG kept or not")
	stored := func(name string) (int, string) {
		id, d, rid := opened(t, alice, e.w.s.storyID, name)
		up := uploadsOf(t, alice.call(t, "amp_add_evidence", map[string]any{"receipt_id": float64(rid), "dispatch_id": d,
			"tree_sha": treeA, "files": []any{file("screenshot-1.png", png)}}))["screenshot-1.png"]
		if code, _ := e.put(t, up.url, png); code != 200 {
			t.Fatalf("upload: %d", code)
		}
		alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(id), "dispatch_id": d, "receipt_id": float64(rid),
			"checks": []any{passCheck("1")}})
		return id, up.key
	}
	unkept, unkeptKey := stored("unkept")
	kept, keptKey := stored("kept")
	fresh, freshKey := stored("fresh")
	e.ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/receipt/accept", kept), nil)
	// A stale pending upload, and a pending one still in its grace day.
	_, dp, rp := opened(t, alice, e.w.s.storyID, "stale upload")
	pend := uploadsOf(t, alice.call(t, "amp_add_evidence", map[string]any{"receipt_id": float64(rp), "dispatch_id": dp,
		"tree_sha": treeA, "files": []any{file("screenshot-1.png", png), file("screenshot-2.png", png)}}))
	execSQL(t, `UPDATE receipt_evidence SET upload_expires_at = NOW() - INTERVAL '25 hours' WHERE key = $1`, pend["screenshot-1.png"].key)

	execSQL(t, `UPDATE task_receipts SET created_at = NOW() - INTERVAL '91 days' WHERE task_id = ANY($1)`, []int{unkept, kept})
	expired, dropped, err := e.svc().SweepReceiptEvidence(ctx)
	if err != nil || expired != 1 || dropped != 1 {
		t.Fatalf("sweep: %d expired, %d dropped, %v", expired, dropped, err)
	}
	if e.inStore(unkeptKey) || evidenceStatus(t, unkeptKey) != "expired" {
		t.Fatal("unkept evidence past retention wasn't expired")
	}
	if !e.inStore(keptKey) || evidenceStatus(t, keptKey) != "stored" || !e.inStore(freshKey) || evidenceStatus(t, freshKey) != "stored" {
		t.Fatal("kept or fresh evidence was expired")
	}
	if evidenceStatus(t, pend["screenshot-1.png"].key) != "none" || evidenceStatus(t, pend["screenshot-2.png"].key) != "pending" {
		t.Fatal("pending uploads swept wrongly")
	}
	if resp, _ := e.get(t, "bob", unkeptKey, ""); resp.StatusCode != 410 {
		t.Fatalf("expired GET: %d, want 410", resp.StatusCode)
	}
	if resp, _ := e.get(t, "bob", keptKey, ""); resp.StatusCode != 200 {
		t.Fatalf("kept GET: %d", resp.StatusCode)
	}
	if ev := getReceipt(t, e.ra, "bob", unkept)["evidence"].([]any); ev[0].(map[string]any)["status"] != "expired" {
		t.Fatalf("receipt evidence after the sweep: %v", ev)
	}
	// Merging keeps a ticket's receipts.
	e.ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/runs", fresh), map[string]any{"merged": true})
	if rc, _ := e.w.s.repo.TaskReceipts(ctx, fresh); len(rc) == 0 || !rc[0].Kept {
		t.Fatalf("merged ticket's receipt not kept: %+v", rc)
	}

	// A ticket purged from the trash takes its receipts and evidence.
	e.w.as(e.w.root).call(t, "amp_delete_task", map[string]any{"task_id": float64(kept)})
	if !e.inStore(keptKey) || receiptRows(t, kept) == 0 {
		t.Fatal("deleting (to the trash) dropped the evidence")
	}
	execSQL(t, `UPDATE deleted_tasks SET deleted_at = NOW() - INTERVAL '31 days' WHERE task_id = $1`, kept)
	if _, err := e.svc().PurgeTrash(ctx); err != nil {
		t.Fatal(err)
	}
	if e.inStore(keptKey) || receiptRows(t, kept) != 0 || evidenceStatus(t, keptKey) != "none" {
		t.Fatal("purge left the ticket's receipts or evidence")
	}

	// A reset deletes the project's receipts and evidence.
	e.ra.want(t, 200, "owner", http.MethodPost, fmt.Sprintf("/api/projects/%d/reset", e.w.s.projectID), nil)
	if e.inStore(freshKey) || receiptRows(t, fresh) != 0 || receiptRows(t, unkept) != 0 {
		t.Fatal("reset left receipts or evidence")
	}
}

// The upload route needs no AMP session (its token is the credential);
// main's auth middleware lets it through, and only it.
func TestReceipts_UploadBypassesSessionAuthOnly(t *testing.T) {
	e := newEvidenceWorld(t, "inform")
	idp := newTestIdP(t)
	v, err := auth.NewVerifier(context.Background(), auth.Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL, Repo: e.w.s.repo})
	if err != nil {
		t.Fatal(err)
	}
	h := api.NewRestHandler(e.w.s.reg, e.w.s.repo, e.w.s.hub, e.w.s.kb)
	h.SetReceiptStore(e.ev)
	handler := authExceptHealth(v, apiMux(h))
	alice := e.w.as(e.w.alice)
	_, d, rid := opened(t, alice, e.w.s.storyID, "upload no session")
	png := []byte("png bytes")
	up := uploadsOf(t, alice.call(t, "amp_add_evidence", map[string]any{"receipt_id": float64(rid), "dispatch_id": d,
		"tree_sha": treeA, "files": []any{file("screenshot-1.png", png)}}))["screenshot-1.png"]
	do := func(method, path string, body []byte) int {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewReader(body)))
		return rec.Code
	}
	if got := do(http.MethodPut, up.url, png); got != 200 {
		t.Fatalf("upload without a session: %d, want 200", got)
	}
	if got := do(http.MethodGet, "/api/receipts/"+up.key, nil); got != http.StatusUnauthorized {
		t.Fatalf("serving without a session: %d, want 401", got)
	}
}

// ---- Fix round 1 ----

// failingStore is a store whose deletes fail (a volume gone read-only).
type failingStore struct{ receipts.Store }

func (failingStore) Delete(context.Context, string) error { return fmt.Errorf("read-only file system") }

// Purge and reset never drop evidence rows they couldn't delete the files
// of: with no store, or when a delete fails, they wait (purge) or say try
// again (reset), and the files and rows stay.
func TestReceipts_PurgeResetKeepEvidenceTheyCantDelete(t *testing.T) {
	e := newEvidenceWorld(t, "inform")
	alice := e.w.as(e.w.alice)
	ctx := context.Background()
	png := []byte("\x89PNG keep me")
	id, d, rid := opened(t, alice, e.w.s.storyID, "to purge")
	up := uploadsOf(t, alice.call(t, "amp_add_evidence", map[string]any{"receipt_id": float64(rid), "dispatch_id": d,
		"tree_sha": treeA, "files": []any{file("screenshot-1.png", png)}}))["screenshot-1.png"]
	if code, _ := e.put(t, up.url, png); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(id), "dispatch_id": d, "receipt_id": float64(rid)})
	e.w.as(e.w.root).call(t, "amp_delete_task", map[string]any{"task_id": float64(id)})
	execSQL(t, `UPDATE deleted_tasks SET deleted_at = NOW() - INTERVAL '31 days' WHERE task_id = $1`, id)

	keep := func(what string) {
		t.Helper()
		if !e.inStore(up.key) || receiptRows(t, id) == 0 || evidenceStatus(t, up.key) != "stored" {
			t.Fatalf("%s: dropped the receipt or its evidence", what)
		}
	}
	// No store.
	noStore := service.New(e.w.s.repo, e.w.s.reg, nil)
	if _, err := noStore.PurgeTrash(ctx); err == nil {
		t.Fatal("purge with no store: no error")
	}
	keep("purge with no store")
	// A delete that fails.
	failing := service.New(e.w.s.repo, e.w.s.reg, nil)
	failing.Evidence = &service.EvidenceStore{Store: failingStore{e.store}}
	if _, err := failing.PurgeTrash(ctx); err == nil {
		t.Fatal("purge with failing deletes: no error")
	}
	keep("purge with failing deletes")
	// A reset with no store says try again.
	bare := newRestAs(t, e.w)
	if code, out := bare.do(t, "owner", http.MethodPost, fmt.Sprintf("/api/projects/%d/reset", e.w.s.projectID), nil); code != 500 ||
		!strings.Contains(fmt.Sprint(out), "try again") {
		t.Fatalf("reset with no store: %d %v", code, out)
	}
	keep("reset with no store")
	// With the store back, the purge goes through.
	if _, err := e.svc().PurgeTrash(ctx); err != nil {
		t.Fatal(err)
	}
	if e.inStore(up.key) || receiptRows(t, id) != 0 {
		t.Fatal("purge with the store back left the evidence")
	}
}

// Two uploads with the same token: while the first runs, the second is
// refused (409) and touches nothing; the first stores its bytes.
func TestReceipts_ConcurrentSameTokenUpload(t *testing.T) {
	e := newEvidenceWorld(t, "inform")
	alice := e.w.as(e.w.alice)
	_, d, rid := opened(t, alice, e.w.s.storyID, "concurrent")
	body := bytes.Repeat([]byte("v"), 64<<10)
	up := uploadsOf(t, alice.call(t, "amp_add_evidence", map[string]any{"receipt_id": float64(rid), "dispatch_id": d,
		"tree_sha": treeA, "files": []any{file("video.mp4", body, "frames", float64(10))}}))["video.mp4"]

	pr, pw := io.Pipe()
	// Runs before the server's Close, so a failure doesn't leave the first
	// request (and Close) waiting on its body.
	t.Cleanup(func() { pw.CloseWithError(fmt.Errorf("test over")) })
	first := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPut, e.ra.server.URL+up.url, pr)
		req.ContentLength = int64(len(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			first <- -1
			return
		}
		resp.Body.Close()
		first <- resp.StatusCode
	}()
	if _, err := pw.Write(body[:1024]); err != nil {
		t.Fatal(err)
	}
	// The first request holds the claim now (its body is being read).
	deadline := time.Now().Add(5 * time.Second)
	for {
		var tok string
		db, err := pgx.Connect(context.Background(), testDSN(t))
		if err != nil {
			t.Fatal(err)
		}
		_ = db.QueryRow(context.Background(), `SELECT upload_token_hash FROM receipt_evidence WHERE key = $1`, up.key).Scan(&tok)
		db.Close(context.Background())
		if strings.Contains(tok, ":claim:") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first upload never claimed the row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, _ := e.put(t, up.url, body); code != 409 {
		t.Fatalf("second upload with the same token: %d, want 409", code)
	}
	if _, err := pw.Write(body[1024:]); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	if code := <-first; code != 200 {
		t.Fatalf("first upload: %d", code)
	}
	if evidenceStatus(t, up.key) != "stored" || !e.inStore(up.key) {
		t.Fatal("first upload not stored")
	}
	if resp, got := e.get(t, "bob", up.key, ""); resp.StatusCode != 200 || !bytes.Equal(got, body) {
		t.Fatalf("stored bytes: %d, %d bytes", resp.StatusCode, len(got))
	}
	if code, _ := e.put(t, up.url, body); code != 404 {
		t.Fatalf("token after the upload: %d, want 404", code)
	}
}

// The upload prefix skips session auth, but a path that only starts with
// it and leads elsewhere never reaches another handler unauthenticated.
func TestReceipts_UploadPrefixTraversal(t *testing.T) {
	e := newEvidenceWorld(t, "inform")
	idp := newTestIdP(t)
	v, err := auth.NewVerifier(context.Background(), auth.Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL, Repo: e.w.s.repo})
	if err != nil {
		t.Fatal(err)
	}
	h := api.NewRestHandler(e.w.s.reg, e.w.s.repo, e.w.s.hub, e.w.s.kb)
	h.SetReceiptStore(e.ev)
	handler := authExceptHealth(v, apiMux(h))
	id := mkIn(t, e.w.as(e.w.alice), e.w.s.storyID, "target")
	for _, path := range []string{
		fmt.Sprintf("/api/receipt-uploads/../tasks/%d", id),
		fmt.Sprintf("/api/receipt-uploads/%%2e%%2e/tasks/%d", id),
		fmt.Sprintf("/api/receipt-uploads/x/../../tasks/%d", id),
		fmt.Sprintf("/api/receipt-uploads/../projects/%d", e.w.s.projectID),
	} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPost, http.MethodPatch, http.MethodDelete} {
			req := httptest.NewRequest(method, "http://amp.test/", bytes.NewReader([]byte(`{}`)))
			req.URL.RawPath = ""
			req.URL.Path, _ = urlUnescape(path)
			req.RequestURI = path
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code == 200 || (rec.Code >= 200 && rec.Code < 300) {
				t.Fatalf("%s %s without a session: %d %s", method, path, rec.Code, rec.Body.String())
			}
		}
	}
	if id2 := taskState(t, e.w.s, id); id2 != domain.TaskStateBacklog {
		t.Fatalf("the ticket changed: %s", id2)
	}
}

func urlUnescape(p string) (string, error) { return url.PathUnescape(p) }

// Off the web UI an item's own receipts values only rise: removing an
// epic's own minimum level or lowering its own mode is refused, even where
// the epic's effective setting wouldn't change (a ticket under it may set
// Require itself). v1 accepts self_checked as the only minimum level.
func TestReceipts_OwnValuesOnlyRiseOffUI(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice, owner := w.as(w.alice), w.as(w.owner)
	setReceiptsMode(t, ra, w.s.projectID, "inform")
	epic := float64(w.s.epicID)
	owner.call(t, "amp_update_epic", map[string]any{"epic_id": epic, "metadata": map[string]any{"receipts_min_level": "self_checked"}})
	id := mkIn(t, alice, w.s.storyID, "require under the epic")
	alice.call(t, "amp_update_task", map[string]any{"task_id": float64(id), "metadata": map[string]any{"receipts": "require"}})
	if s := getReceipt(t, ra, "alice", id)["settings"].(map[string]any); s["mode"] != "require" || s["min_level"] != "self_checked" {
		t.Fatalf("settings: %v", s)
	}
	wantErr(t, owner.callExpectError(t, "amp_update_epic", map[string]any{"epic_id": epic, "metadata": map[string]any{"receipts_min_level": nil}}),
		"web UI")
	// independently_verified isn't available in v1, as metadata or as the
	// project's minimum.
	wantErr(t, owner.callExpectError(t, "amp_update_epic", map[string]any{"epic_id": epic,
		"metadata": map[string]any{"receipts_min_level": "independently_verified"}}), "self_checked only")
	if code, out := ra.do(t, "owner", http.MethodPatch, fmt.Sprintf("/api/projects/%d", w.s.projectID),
		map[string]any{"receipts_min_level": "independently_verified"}); code != 400 || !strings.Contains(fmt.Sprint(out), "self_checked only") {
		t.Fatalf("project minimum independently_verified: %d %v", code, out)
	}
	// An own mode only rises too, even where the effective one doesn't change.
	owner.call(t, "amp_update_epic", map[string]any{"epic_id": epic, "metadata": map[string]any{"receipts": "require"}})
	wantErr(t, owner.callExpectError(t, "amp_update_epic", map[string]any{"epic_id": epic, "metadata": map[string]any{"receipts": "inform"}}), "web UI")
	wantErr(t, owner.callExpectError(t, "amp_update_epic", map[string]any{"epic_id": epic, "metadata": map[string]any{"receipts": nil}}), "web UI")
	// A person in the web UI may lower or remove them.
	ra.want(t, 200, "owner", http.MethodPatch, fmt.Sprintf("/api/epics/%d", w.s.epicID),
		map[string]any{"metadata": map[string]any{"receipts_min_level": nil, "receipts": nil}})
	if s := getReceipt(t, ra, "alice", id)["settings"].(map[string]any); s["mode"] != "require" {
		t.Fatalf("after the UI change: %v", s)
	}
}

// A new project can't weaken the org default off the web UI, and an import
// off the web UI brings no receipts metadata.
func TestReceipts_NewAndImportedProjects(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	sfx := fmt.Sprint(time.Now().UnixNano())
	ra.want(t, 200, "orgadmin", http.MethodPut, fmt.Sprintf("/api/orgs/%d/receipts", w.org.ID), map[string]any{"mode": "require"})
	admin := w.as(w.orgadmin)
	org := float64(w.org.ID)
	wantErr(t, admin.callExpectError(t, "amp_create_project", map[string]any{"name": "weak", "code": "rw-" + sfx, "org_id": org,
		"metadata": map[string]any{"receipts": "off"}}), "web UI")
	wantErr(t, admin.callExpectError(t, "amp_create_project", map[string]any{"name": "bad", "code": "rb-" + sfx, "org_id": org,
		"metadata": map[string]any{"receipts": "sometimes"}}), "off, inform or require")
	admin.call(t, "amp_create_project", map[string]any{"name": "strict", "code": "rs-" + sfx, "org_id": org,
		"metadata": map[string]any{"receipts": "require", "receipts_min_level": "self_checked"}})
	// REST: an API token is refused; the web UI may.
	if code, _ := ra.doVia(t, auth.ChannelToken, "orgadmin", http.MethodPost, "/api/projects",
		map[string]any{"name": "weak", "code": "rt-" + sfx, "org_id": w.org.ID, "metadata": map[string]any{"receipts": "inform"}}); code != 403 {
		t.Fatalf("REST create via a token: %d", code)
	}
	ra.want(t, 200, "orgadmin", http.MethodPost, "/api/projects",
		map[string]any{"name": "ui weak", "code": "ru-" + sfx, "org_id": w.org.ID, "metadata": map[string]any{"receipts": "inform"}})

	// Import off the web UI: every receipts key is dropped, the rest kept.
	bundle := map[string]any{"version": "1",
		"project": map[string]any{"name": "imp", "code": "ri-" + sfx, "metadata": map[string]any{"receipts": "off", "team": "x"}},
		"epics":   []any{map[string]any{"id": 1, "name": "e", "priority": "1", "metadata": map[string]any{"receipts_min_level": "self_checked"}}},
		"stories": []any{map[string]any{"id": 2, "epic_id": 1, "name": "s", "priority": "1", "metadata": map[string]any{"receipts": "off"}}},
		"tasks":   []any{map[string]any{"id": 3, "epic_id": 1, "story_id": 2, "name": "t", "priority": "1", "metadata": map[string]any{"receipts": "inform"}}},
	}
	raw, _ := json.Marshal(bundle)
	out := admin.call(t, "amp_import_project", map[string]any{"bundle_json": string(raw), "code": "ri-" + sfx, "org_id": org})
	if intField(t, out, "receipts_keys_removed") != 4 {
		t.Fatalf("import: %v", out)
	}
	pid := intField(t, out["project"].(map[string]any), "id")
	ctx := context.Background()
	proj, _ := w.s.repo.GetProject(ctx, pid)
	if _, ok := proj.Metadata["receipts"]; ok || proj.Metadata["team"] != "x" {
		t.Fatalf("imported project metadata: %v", proj.Metadata)
	}
	tasks, _ := w.s.repo.ListTasks(ctx, pid, "")
	for _, tk := range tasks {
		if _, ok := tk.Metadata["receipts"]; ok {
			t.Fatalf("imported ticket metadata: %v", tk.Metadata)
		}
	}
	if len(tasks) != 1 {
		t.Fatalf("imported %d tickets", len(tasks))
	}
}

// Off the web UI nothing completes a held ticket: not its worker again with
// a passing check, not a closeout, not set_task_state or a REST call with a
// token. It stays held, with one receipt, until a person acts.
func TestReceipts_HeldTicketWaitsForAPerson(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	setReceiptsMode(t, ra, w.s.projectID, "require")
	x := mkIn(t, alice, w.s.storyID, "held")
	dx := dispatchClaims(t, alice, x)
	if out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(x), "dispatch_id": dx}); out["held"] != true {
		t.Fatalf("no checks under Require: %v", out)
	}
	pass := []any{passCheck("1")}
	wantErr(t, alice.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(x), "checks": pass}), "is held", "web UI")
	wantErr(t, alice.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(x), "dispatch_id": dx, "checks": pass}), "is held")
	wantErr(t, w.as(w.orgadmin).callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(x), "checks": pass,
		"closeout_reason": "looks done"}), "is held")
	wantErr(t, alice.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(x), "closeout_reason": "stuck"}), "is held")
	wantErr(t, alice.callExpectError(t, "amp_set_task_state", map[string]any{"task_id": float64(x), "state": "completed"}), "is held")
	if code, out := ra.doVia(t, auth.ChannelToken, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", x), nil); code != 409 {
		t.Fatalf("REST complete with a token: %d %v", code, out)
	}
	if st := taskState(t, alice, x); st != domain.TaskStateInProgress {
		t.Fatalf("held ticket is %s", st)
	}
	if got := getReceipt(t, ra, "alice", x); got["held"] != true {
		t.Fatalf("receipt: %v", got)
	}
	if n := receiptRows(t, x); n != 1 {
		t.Fatalf("refused completions made %d receipts", n)
	}
	// A person accepts it in the web UI.
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/receipt/accept", x), nil)
	if st := taskState(t, alice, x); st != domain.TaskStateCompleted {
		t.Fatalf("accepted ticket is %s", st)
	}
	// Rework: a person rejects it (back to ready), and a new dispatch's
	// worker completes it.
	y := mkIn(t, alice, w.s.storyID, "rework")
	dy := dispatchClaims(t, alice, y)
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(y), "dispatch_id": dy})
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/receipt/reject", y), map[string]any{"note": "add the test"})
	dy2 := dispatchClaims(t, alice, y)
	if out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(y), "dispatch_id": dy2, "checks": pass}); out["state"] != "completed" {
		t.Fatalf("rework: %v", out)
	}
}

// Moving a project to another org off the web UI can't weaken its receipts
// setting: one that follows a Require org's default can't move to an Off
// org. Into another Require org, or in the web UI, it can.
func TestReceipts_OrgMoveCantWeaken(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	ra.want(t, 200, "orgadmin", http.MethodPut, fmt.Sprintf("/api/orgs/%d/receipts", w.org.ID), map[string]any{"mode": "require"})
	root := w.as(w.root)
	move := func(org int) map[string]any {
		return map[string]any{"project_id": float64(w.s.projectID), "action": "set_org", "org_id": float64(org)}
	}
	wantErr(t, root.callExpectError(t, "amp_manage_project_teams", move(w.other.ID)), "web UI", "require")
	if code, out := ra.doVia(t, auth.ChannelToken, "root", http.MethodPost, fmt.Sprintf("/api/projects/%d/org", w.s.projectID),
		map[string]any{"org_id": w.other.ID}); code != 403 {
		t.Fatalf("REST move with a token: %d %v", code, out)
	}
	if p, _ := w.s.repo.GetProject(context.Background(), w.s.projectID); p.OrgID == nil || *p.OrgID != w.org.ID {
		t.Fatalf("project moved: %v", p.OrgID)
	}
	ra.want(t, 200, "outsider", http.MethodPut, fmt.Sprintf("/api/orgs/%d/receipts", w.other.ID), map[string]any{"mode": "require"})
	root.call(t, "amp_manage_project_teams", move(w.other.ID))
	if p, _ := w.s.repo.GetProject(context.Background(), w.s.projectID); p.OrgID == nil || *p.OrgID != w.other.ID {
		t.Fatalf("move into another Require org: %v", p.OrgID)
	}
}

// Strictly held: off the web UI an agent can't move a held ticket to any
// other state (back to ready included), block, return or delete it; it
// stays held. A person moving it back in the web UI ends the hold.
func TestReceipts_HeldTicketCantBeMovedOffUI(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	setReceiptsMode(t, ra, w.s.projectID, "require")
	x := mkIn(t, alice, w.s.storyID, "held")
	dx := dispatchClaims(t, alice, x)
	if out := alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(x), "dispatch_id": dx}); out["held"] != true {
		t.Fatalf("not held: %v", out)
	}
	id := float64(x)
	wantErr(t, alice.callExpectError(t, "amp_set_task_state", map[string]any{"task_id": id, "state": "backlog"}), "is held")
	wantErr(t, alice.callExpectError(t, "amp_set_task_state", map[string]any{"task_id": id, "state": "blocked", "reason": "stuck"}), "is held")
	wantErr(t, alice.callExpectError(t, "amp_block_task", map[string]any{"task_id": id, "reason": "should I?"}), "is held")
	wantErr(t, alice.callExpectError(t, "amp_return_task", map[string]any{"task_id": id, "reason": "worker died", "dispatch_id": dx}), "is held")
	wantErr(t, alice.callExpectError(t, "amp_delete_task", map[string]any{"task_id": id}), "is held")
	wantErr(t, w.as(w.root).callExpectError(t, "amp_delete_story", map[string]any{"story_id": float64(w.s.storyID)}), "is held")
	if code, out := ra.doVia(t, auth.ChannelToken, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/return", x),
		map[string]any{"reason": "back to ready"}); code != 409 {
		t.Fatalf("REST return with a token: %d %v", code, out)
	}
	if st := taskState(t, alice, x); st != domain.TaskStateInProgress {
		t.Fatalf("held ticket moved to %s", st)
	}
	if got := getReceipt(t, ra, "alice", x); got["held"] != true {
		t.Fatalf("hold lost: %v", got)
	}
	// Nor does the not-started watchdog return it, however long it waits.
	execSQL(t, `UPDATE tasks SET dispatched_at = NOW() - INTERVAL '2 hours', started_at = NULL WHERE id = $1`, x)
	if _, err := service.New(w.s.repo, w.s.reg, nil).SweepNotStarted(context.Background(), allowAll); err != nil {
		t.Fatal(err)
	}
	if st := taskState(t, alice, x); st != domain.TaskStateInProgress || getReceipt(t, ra, "alice", x)["held"] != true {
		t.Fatalf("the watchdog released a held ticket: %s", st)
	}
	// The refusals are audited.
	entries, err := w.s.repo.ListAudit(context.Background(), domain.AuditFilter{ProjectID: w.s.projectID, Action: "task.state.denied"})
	if err != nil || len(entries) == 0 {
		t.Fatalf("no audit for the refused move: %v %v", entries, err)
	}
	// Other in-progress tickets move as before.
	other := mkIn(t, alice, w.s.storyID, "not held")
	dispatchClaims(t, alice, other)
	alice.call(t, "amp_set_task_state", map[string]any{"task_id": float64(other), "state": "backlog"})
	// A person in the web UI moves it back; that ends the hold.
	uiSetState(t, w, w.alice, x, domain.TaskStateBacklog)
	if st := taskState(t, alice, x); st != domain.TaskStateBacklog {
		t.Fatalf("UI move: %s", st)
	}
	if got := getReceipt(t, ra, "alice", x); got["held"] != false {
		t.Fatalf("hold after the UI move: %v", got)
	}
}

// A receipt-level tree_sha that isn't a full tree id is refused.
func TestReceipts_CompletionTreeSHAValidated(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	setReceiptsMode(t, ra, w.s.projectID, "inform")
	x := mkIn(t, alice, w.s.storyID, "short tree")
	dx := dispatchClaims(t, alice, x)
	wantErr(t, alice.callExpectError(t, "amp_complete_task", map[string]any{"task_id": float64(x), "dispatch_id": dx,
		"tree_sha": "abc1234", "checks": []any{passCheck("1")}}), "12 to 64 hex digits")
	if st := taskState(t, alice, x); st != domain.TaskStateInProgress {
		t.Fatalf("refused completion left it %s", st)
	}
}

// uiSetState sets a ticket's state as a person in the web UI would (the
// service's SetTaskState with a web UI principal; the board has no REST
// route for an arbitrary state).
func uiSetState(t *testing.T, w *rbacWorld, who *auth.Identity, id int, state domain.TaskState) {
	t.Helper()
	ident := *who
	ident.Listener, ident.Channel = "rest", auth.ChannelUI
	ctx := auth.WithIdentity(context.Background(), &ident)
	p, err := authz.FromContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !p.InUI() {
		t.Fatal("not a web UI principal")
	}
	if _, err := service.New(w.s.repo, w.s.reg, nil).SetTaskState(ctx, p, id, state, ""); err != nil {
		t.Fatalf("UI state %s: %v", state, err)
	}
}

// receiptStoreFromEnv with RECEIPT_STORE=s3: a bad config leaves the store
// out (evidence calls refuse, amp-api runs on); a store that can't be
// reached yet is kept, to be tried again at the first upload; refused
// credentials leave it out. The secret is never in the error.
func TestReceiptStoreFromEnv_S3(t *testing.T) {
	set := func(kv map[string]string) {
		for _, k := range []string{"RECEIPT_STORE", "RECEIPT_S3_ENDPOINT", "RECEIPT_S3_BUCKET", "RECEIPT_S3_ACCESS_KEY",
			"RECEIPT_S3_SECRET_KEY", "RECEIPT_S3_USE_SSL", "RECEIPT_S3_SPOOL_DIR"} {
			t.Setenv(k, kv[k])
		}
	}
	set(map[string]string{"RECEIPT_STORE": "s3", "RECEIPT_S3_ENDPOINT": "127.0.0.1:1", "RECEIPT_S3_SECRET_KEY": "hunter2-secret"})
	if e := receiptStoreFromEnv(); e.Store != nil || !strings.Contains(e.Err, "RECEIPT_S3_BUCKET") || strings.Contains(e.Err, "hunter2") {
		t.Fatalf("missing bucket: %+v", e)
	}
	set(map[string]string{"RECEIPT_STORE": "s3", "RECEIPT_S3_ENDPOINT": "127.0.0.1:1", "RECEIPT_S3_BUCKET": "ev", "RECEIPT_S3_USE_SSL": "maybe",
		"RECEIPT_S3_ACCESS_KEY": "a", "RECEIPT_S3_SECRET_KEY": "s"})
	if e := receiptStoreFromEnv(); e.Store != nil || !strings.Contains(e.Err, "RECEIPT_S3_USE_SSL") {
		t.Fatalf("malformed boolean: %+v", e)
	}
	set(map[string]string{"RECEIPT_STORE": "s3", "RECEIPT_S3_ENDPOINT": "127.0.0.1:1", "RECEIPT_S3_BUCKET": "ev", "RECEIPT_S3_USE_SSL": "false",
		"RECEIPT_S3_ACCESS_KEY": "a", "RECEIPT_S3_SECRET_KEY": "s", "RECEIPT_S3_SPOOL_DIR": t.TempDir()})
	if e := receiptStoreFromEnv(); e.Store == nil || e.Err != "" {
		t.Fatalf("store not up yet: %+v, want it kept", e)
	}
	ep := os.Getenv("RECEIPT_S3_TEST_ENDPOINT")
	if ep == "" {
		return
	}
	set(map[string]string{"RECEIPT_STORE": "s3", "RECEIPT_S3_ENDPOINT": ep, "RECEIPT_S3_BUCKET": s3TestBucket + "-env", "RECEIPT_S3_USE_SSL": "false",
		"RECEIPT_S3_ACCESS_KEY": "nobody", "RECEIPT_S3_SECRET_KEY": "hunter2-secret", "RECEIPT_S3_SPOOL_DIR": t.TempDir()})
	if e := receiptStoreFromEnv(); e.Store != nil || e.Err == "" || strings.Contains(e.Err, "hunter2") {
		t.Fatalf("refused credentials: %+v", e)
	}
	set(map[string]string{"RECEIPT_STORE": "s3", "RECEIPT_S3_ENDPOINT": ep, "RECEIPT_S3_BUCKET": s3TestBucket, "RECEIPT_S3_USE_SSL": "false",
		"RECEIPT_S3_ACCESS_KEY": os.Getenv("RECEIPT_S3_TEST_ACCESS_KEY"), "RECEIPT_S3_SECRET_KEY": os.Getenv("RECEIPT_S3_TEST_SECRET_KEY"),
		"RECEIPT_S3_SPOOL_DIR": t.TempDir()})
	if e := receiptStoreFromEnv(); e.Store == nil || e.Err != "" {
		t.Fatalf("s3 store: %+v", e)
	}
}
