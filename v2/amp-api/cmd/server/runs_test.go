package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Agents record what each run used and where its work went; reports total
// the spend (by model and by metadata, e.g. capitalizable), and a commit or
// branch leads back to the ticket.
func TestRuns_UsageReportsAndTrace(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	root := w.s.as(w.root)
	// Unique per run: root sees every project in the shared test database.
	sha := fmt.Sprintf("%016x", time.Now().UnixNano())
	branch, pr := "checkout-"+sha, "https://git.example/pr/"+sha
	root.call(t, "amp_update_epic", map[string]any{"epic_id": float64(w.s.epicID), "metadata": map[string]any{"capitalizable": true}})
	a := mkIn(t, root, w.s.storyID, "build checkout")
	b := mkIn(t, root, w.s.storyID, "fix flaky test")
	root.call(t, "amp_update_task", map[string]any{"task_id": float64(b), "metadata": map[string]any{"capitalizable": false}})

	// A run that died, reported afterwards, then the run that finished.
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "amp-worker-backend"})
	root.call(t, "amp_report_run", map[string]any{"task_id": float64(a), "outcome": "abandoned", "model": "deepseek-v4-flash",
		"input_tokens": 1000.0, "output_tokens": 100.0, "run_note": "stalled mid-stream"})
	done := root.call(t, "amp_complete_task", map[string]any{"task_id": float64(a), "model": "kimi-k3",
		"input_tokens": 40000.0, "output_tokens": 5000.0, "cache_read_tokens": 90000.0, "cost_usd": 0.42,
		"branch": branch, "commit": strings.ToUpper(sha), "pushed": true, "pr_url": pr})
	if done["run"] == nil {
		t.Fatalf("completion didn't record its run: %v", done)
	}
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(b), "agent_id": "amp-worker-backend"})
	root.call(t, "amp_complete_task", map[string]any{"task_id": float64(b), "model": "kimi-k3", "input_tokens": 500.0, "output_tokens": 50.0})

	runs := root.call(t, "amp_get_task_runs", map[string]any{"task_id": float64(a)})
	u := runs["usage"].(map[string]any)
	if len(runs["runs"].([]any)) != 2 || u["input_tokens"] != 41000.0 || u["output_tokens"] != 5100.0 {
		t.Fatalf("ticket runs: %v", runs)
	}

	// History tells the whole story in one call.
	hist := root.call(t, "amp_get_ticket_history", map[string]any{"task_id": float64(a)})
	if len(hist["runs"].([]any)) != 2 || hist["comments"] == nil {
		t.Fatalf("history without runs/comments: %v", hist)
	}

	// Report: totals, by model, and split by the capitalizable metadata.
	rep := ra.want(t, 200, "root", http.MethodGet, fmt.Sprintf("/api/projects/%d/report?group_by=capitalizable", w.s.projectID), nil)
	if tu := rep["usage"].(map[string]any); tu["input_tokens"] != 41500.0 || tu["runs"] != 3.0 {
		t.Fatalf("report usage: %v", tu)
	}
	// Spend per person: both tickets are root's.
	people := rep["people"].([]any)
	if len(people) != 1 || people[0].(map[string]any)["key"] != fmt.Sprint(w.root.User.ID) || people[0].(map[string]any)["usage"] == nil {
		t.Fatalf("people: %v", people)
	}
	if len(rep["usage_by_model"].([]any)) != 2 {
		t.Fatalf("by model: %v", rep["usage_by_model"])
	}
	split := map[string]float64{}
	for _, g := range rep["breakdown"].([]any) {
		m := g.(map[string]any)
		if us, ok := m["usage"].(map[string]any); ok {
			split[m["key"].(string)] = us["input_tokens"].(float64)
		}
	}
	if split["true"] != 41000 || split["false"] != 500 {
		t.Fatalf("capitalizable split: %v", split)
	}

	// Trace: a short SHA (any case) or the branch leads back to the ticket.
	for _, q := range []map[string]any{{"commit": sha[:9]}, {"branch": branch}, {"pr_url": pr}} {
		tr := root.call(t, "amp_trace", q)
		hits := tr["tickets"].([]any)
		if len(hits) != 1 || int(hits[0].(map[string]any)["task"].(map[string]any)["id"].(float64)) != a {
			t.Fatalf("trace %v: %v", q, tr)
		}
	}
	if err := root.callExpectError(t, "amp_trace", map[string]any{"commit": "abc"}); err == "" {
		t.Fatal("a 3-character SHA prefix should be refused")
	}
	// Someone outside the project finds nothing.
	out := ra.want(t, 200, "outsider", http.MethodGet, "/api/trace?commit="+sha[:9], nil)
	if out["count"] != 0.0 {
		t.Fatalf("outsider traced into a project they can't see: %v", out)
	}
	// Finished but not merged: a waits on a person; marking it merged clears it.
	um := root.call(t, "amp_list_unmerged", map[string]any{"project_id": float64(w.s.projectID)})
	if um["count"] != 1.0 || int(um["waiting_to_merge"].([]any)[0].(map[string]any)["task_id"].(float64)) != a {
		t.Fatalf("waiting to merge: %v", um)
	}
	ra.want(t, 200, "root", http.MethodPost, fmt.Sprintf("/api/tasks/%d/runs", a), map[string]any{"outcome": "completed", "merged": true, "note": "merged by a person"})
	if um := ra.want(t, 200, "root", http.MethodGet, fmt.Sprintf("/api/projects/%d/unmerged", w.s.projectID), nil); um["count"] != 0.0 {
		t.Fatalf("still waiting after merge: %v", um)
	}

	// CI reports the merge by branch, with a token that can only read the project.
	b2 := mkIn(t, root, w.s.storyID, "second feature")
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(b2), "agent_id": "w"})
	root.call(t, "amp_complete_task", map[string]any{"task_id": float64(b2), "branch": branch + "-2", "pushed": true})
	res := ra.want(t, 200, "carol", http.MethodPost, "/api/trace/merged", map[string]any{"branch": branch + "-2", "merge_commit": "fedcba9876543"})
	if res["count"] != 1.0 {
		t.Fatalf("CI merge report: %v", res)
	}
	if um := ra.want(t, 200, "root", http.MethodGet, fmt.Sprintf("/api/projects/%d/unmerged", w.s.projectID), nil); um["count"] != 0.0 {
		t.Fatalf("still waiting after CI reported the merge: %v", um)
	}

	// Recording a run needs the right to work the ticket.
	if err := w.s.as(w.outsider).callExpectError(t, "amp_report_run", map[string]any{"task_id": float64(a), "model": "x"}); err == "" {
		t.Fatal("an outsider recorded a run")
	}
}

// An org's portfolio puts its projects side by side with spend split by the
// capitalizable metadata; people outside the org can't read it.
func TestPortfolio_OrgSpendCapex(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	root := w.s.as(w.root)
	root.call(t, "amp_update_epic", map[string]any{"epic_id": float64(w.s.epicID), "metadata": map[string]any{"capitalizable": true}})
	a := mkIn(t, root, w.s.storyID, "feature")
	b := mkIn(t, root, w.s.storyID, "ops chore")
	root.call(t, "amp_update_task", map[string]any{"task_id": float64(b), "metadata": map[string]any{"capitalizable": false}})
	for id, n := range map[int]float64{a: 1000, b: 10} {
		root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(id), "agent_id": "w"})
		root.call(t, "amp_complete_task", map[string]any{"task_id": float64(id), "model": "m", "input_tokens": n, "output_tokens": n, "cost_usd": n / 1000})
	}
	orgID := w.org.ID
	pf := ra.want(t, 200, "orgadmin", http.MethodGet, fmt.Sprintf("/api/orgs/%d/portfolio?split_by=capitalizable", orgID), nil)
	var row map[string]any
	for _, p := range pf["projects"].([]any) {
		if m := p.(map[string]any); int(m["project_id"].(float64)) == w.s.projectID {
			row = m
		}
	}
	if row == nil {
		t.Fatalf("project missing from the portfolio: %v", pf)
	}
	split := row["split"].(map[string]any)
	if split["true"].(map[string]any)["usage"].(map[string]any)["input_tokens"] != 1000.0 ||
		split["false"].(map[string]any)["usage"].(map[string]any)["input_tokens"] != 10.0 {
		t.Fatalf("split by capitalizable: %v", split)
	}
	keys := fmt.Sprint(pf["ticket_keys"])
	if !strings.Contains(keys, "capitalizable") {
		t.Fatalf("ticket keys offered for splitting: %v", keys)
	}
	if code, _ := ra.do(t, "outsider", http.MethodGet, fmt.Sprintf("/api/orgs/%d/portfolio", orgID), nil); code != 404 && code != 403 {
		t.Fatalf("outsider read the portfolio: %d", code)
	}
}
