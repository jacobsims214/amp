package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// Plan approval covers only what the person saw (AMP #2192). The UI sends
// approve-plan {"as_of": <the newest planned_at on the page>}: tickets
// created, restored or re-scoped after it are not approved and keep awaiting
// (listed in the response's awaiting_approval). No body approves the
// caller's own tickets as they stand now. Approval is per ticket (#2198):
// root planned every ticket here, so root approves them.
func TestPlanApproval_AsOfCoversOnlyWhatWasSeen(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	root := w.s.as(w.root)
	approvalOn(t, root)
	pid := w.s.projectID
	path := fmt.Sprintf("/api/projects/%d/approve-plan", pid)
	ctx := context.Background()

	ticket := func(id int) *domain.Task {
		t.Helper()
		tk, err := w.s.repo.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	planned := func(id int) time.Time { return ticket(id).PlannedAt }
	awaiting := func(out map[string]any) []int {
		t.Helper()
		raw, ok := out["awaiting_approval"].([]any)
		if !ok {
			t.Fatalf("awaiting_approval missing or not a list: %v", out)
		}
		ids := []int{}
		for _, v := range raw {
			ids = append(ids, int(v.(float64)))
		}
		return ids
	}
	approved := func(out map[string]any) []int {
		t.Helper()
		ids := []int{}
		for _, v := range out["approved"].([]any) {
			ids = append(ids, int(v.(float64)))
		}
		return ids
	}
	same := func(got []int, want ...int) bool { return fmt.Sprint(got) == fmt.Sprint(append([]int{}, want...)) }
	asOf := func(ts time.Time) map[string]any { return map[string]any{"as_of": ts.Format(time.RFC3339Nano)} }

	// The person's page shows a; b is created after the page loaded.
	a := mkIn(t, root, w.s.storyID, "seen")
	seen := planned(a)
	b := mkIn(t, root, w.s.storyID, "not seen")
	if !planned(b).After(seen) {
		t.Fatalf("b planned %v, not after a %v", planned(b), seen)
	}

	// Approving as of what was seen approves a, not b: b still awaits.
	out := ra.want(t, 200, "root", http.MethodPost, path, asOf(seen))
	if got := awaiting(out); !same(got, b) {
		t.Fatalf("awaiting after as_of approval = %v, want [%d]", got, b)
	}
	if got := approved(out); !same(got, a) {
		t.Fatalf("approved = %v, want [%d]", got, a)
	}
	aApproved := ticket(a).ApprovedAt
	if aApproved == nil || aApproved.Before(seen) || ticket(a).ApprovedBy == nil || *ticket(a).ApprovedBy != w.root.User.ID {
		t.Fatalf("a approved at %v by %v", aApproved, ticket(a).ApprovedBy)
	}
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "w"})
	wantErr(t, root.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(b), "agent_id": "w"}), "approved")
	check := root.call(t, "amp_check_plan", map[string]any{"project_id": float64(pid)})
	if !containsID(check["awaiting_approval"], b) || containsID(check["awaiting_approval"], a) {
		t.Fatalf("plan check after as_of approval: %v", check["awaiting_approval"])
	}

	// A stale page (as_of older than anything awaiting) approves nothing and
	// never un-approves: a keeps its approval.
	out = ra.want(t, 200, "root", http.MethodPost, path, asOf(seen.Add(-time.Hour)))
	if got := approved(out); len(got) != 0 || !ticket(a).ApprovedAt.Equal(*aApproved) {
		t.Fatalf("stale as_of approved %v; a approved at %v (was %v)", got, ticket(a).ApprovedAt, aApproved)
	}

	// A re-scoped ticket is planned again: an approval as of before the edit
	// doesn't cover it.
	root.call(t, "amp_update_task", map[string]any{"task_id": float64(b), "description": "changed scope"})
	rescoped := planned(b)
	out = ra.want(t, 200, "root", http.MethodPost, path, asOf(seen))
	if got := awaiting(out); !same(got, b) {
		t.Fatalf("awaiting after re-scope = %v, want [%d]", got, b)
	}
	// Seeing the re-scoped ticket and approving as of it approves it.
	out = ra.want(t, 200, "root", http.MethodPost, path, asOf(rescoped))
	if got := awaiting(out); !same(got) {
		t.Fatalf("awaiting after approving as of the re-scope = %v", got)
	}
	if !ticket(b).Approved() {
		t.Fatalf("b not approved after approving as of its re-scope: %+v", ticket(b))
	}

	// as_of in the future approves what exists now, stamped now; a ticket
	// planned later awaits.
	c0 := mkIn(t, root, w.s.storyID, "before a future as_of")
	ra.want(t, 200, "root", http.MethodPost, path, asOf(time.Now().Add(24*time.Hour)))
	if at := ticket(c0).ApprovedAt; at == nil || at.After(time.Now().Add(time.Minute)) {
		t.Fatalf("future as_of stamped %v", at)
	}
	c := mkIn(t, root, w.s.storyID, "after a future as_of")
	check = root.call(t, "amp_check_plan", map[string]any{"project_id": float64(pid)})
	if !containsID(check["awaiting_approval"], c) {
		t.Fatalf("ticket created after a future as_of was approved: %v", check["awaiting_approval"])
	}

	// A malformed as_of is a 400 and approves nothing.
	if code, out := ra.do(t, "root", http.MethodPost, path, map[string]any{"as_of": "yesterday"}); code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "as_of") {
		t.Fatalf("bad as_of: %d %v", code, out)
	}
	if ticket(c).ApprovedAt != nil {
		t.Fatal("a refused approve-plan approved a ticket")
	}

	// No body, or a body without as_of, approves the caller's own tickets as
	// they stand now (backward compatible for one person): nothing awaits.
	for _, body := range []any{nil, map[string]any{}} {
		mkIn(t, root, w.s.storyID, "approved by a body-less approve")
		out = ra.want(t, 200, "root", http.MethodPost, path, body)
		if got := awaiting(out); !same(got) {
			t.Fatalf("awaiting after approve with body %v = %v", body, got)
		}
		if out["project"] == nil {
			t.Fatalf("approve-plan response lost project: %v", out)
		}
	}
}

// planned_at is written on the statement's clock, not the transaction's
// start (AMP #2192): a row inserted late in a long transaction gets the time
// it was written, so an approval given meanwhile doesn't cover it.
func TestPlannedAtUsesClockTimestamp(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	db, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	var def string
	if err := db.QueryRow(ctx, `SELECT column_default FROM information_schema.columns
		WHERE table_name = 'tasks' AND column_name = 'planned_at'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "clock_timestamp") {
		t.Fatalf("tasks.planned_at default = %q, want clock_timestamp()", def)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var started, plannedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_sleep(0.2)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'late in a long transaction')
		RETURNING planned_at`, w.s.projectID, w.s.epicID, w.s.storyID).Scan(&plannedAt); err != nil {
		t.Fatal(err)
	}
	if plannedAt.Sub(started) < 150*time.Millisecond {
		t.Fatalf("planned_at %v is the transaction start %v, not the write time", plannedAt, started)
	}
}
