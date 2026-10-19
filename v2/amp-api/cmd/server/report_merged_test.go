package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/mcp"
)

// tokenMCPServer serves the real MCP listener (newMCPHTTPHandler) behind
// the bearer middleware that accepts AMP API tokens, as Envoy's /mcp route
// reaches it.
func tokenMCPServer(t *testing.T, w *rbacWorld) string {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := newMCPHTTPHandler(newMCPServer(mcp.NewServer(w.s.reg, w.s.repo, w.s.hub, w.s.kb)),
		auth.NewAPITokenVerifier(w.s.repo), mcpHTTPConfig{PublicURL: srv.URL + "/mcp"})
	mux.Handle("/mcp", http.StripPrefix("/mcp", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/" + strings.TrimPrefix(r.URL.Path, "/")
		h.ServeHTTP(rw, r)
	})))
	return srv.URL + "/mcp"
}

// One stateless 2026-07-28 tools/call: no initialize, no session.
func tokenToolCall(t *testing.T, url, token, tool string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	h := modernHeaders("tools/call", tool)
	h["Authorization"] = "Bearer " + token
	r := post(t, url, h, modernToolCall(1, tool, args))
	if r.status != 200 {
		t.Fatalf("%s: HTTP %d: %s", tool, r.status, r.raw)
	}
	if e, ok := r.body["error"]; ok {
		return map[string]any{"error": e}, true
	}
	res := result(t, r)
	if isErr, _ := res["isError"].(bool); isErr {
		return map[string]any{"error": toolText(t, r)}, true
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(toolText(t, r)), &out); err != nil {
		t.Fatalf("%s: %v: %s", tool, err, r.raw)
	}
	return out, false
}

// CI reports a merge over MCP with an amp_ token: one call marks every
// ticket whose runs produced the branch (or PR, or commit) merged. A token
// that can only read the project is enough, as with POST /api/trace/merged.
func TestReportMerged_CITokenOverMCP(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	url := tokenMCPServer(t, w)
	root := w.s.as(w.root)
	sfx := fmt.Sprintf("%016x", time.Now().UnixNano())
	// The SHA must not share a prefix with other tests' time-based SHAs:
	// root (in runs_test.go) traces across every project in the test DB.
	branch, pr, sha := "feat/merge-"+sfx, "https://git.example/pr/"+sfx, "d0c5"+sfx

	// Two tickets of alice's on one branch, one on another PR, one untouched.
	alice := w.s.as(w.alice)
	var onBranch []int
	for _, name := range []string{"api", "ui"} {
		id := mkIn(t, alice, w.s.storyID, name)
		alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(id), "agent_id": "w"})
		alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(id), "branch": branch, "pushed": true})
		onBranch = append(onBranch, id)
	}
	byPR := mkIn(t, alice, w.s.storyID, "docs")
	alice.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(byPR), "agent_id": "w"})
	alice.call(t, "amp_complete_task", map[string]any{"task_id": float64(byPR), "pr_url": pr, "commit": sha, "pushed": true})
	unmerged := func() int {
		um := root.call(t, "amp_list_unmerged", map[string]any{"project_id": float64(w.s.projectID)})
		return int(um["count"].(float64))
	}
	if n := unmerged(); n != 3 {
		t.Fatalf("waiting to merge before CI: %d, want 3", n)
	}

	// carol is on the project but owns none of these: read access only.
	carolTok := mintToken(t, ra, "carol", "me")
	out, isErr := tokenToolCall(t, url, carolTok, "amp_report_merged", map[string]any{"branch": branch, "merge_commit": "fedcba9876543"})
	if isErr || out["count"] != 2.0 {
		t.Fatalf("CI merge by branch: %v", out)
	}
	got := fmt.Sprint(out["merged_task_ids"])
	for _, id := range onBranch {
		if !strings.Contains(got, fmt.Sprint(id)) {
			t.Fatalf("ticket %d not marked: %v", id, got)
		}
	}
	if n := unmerged(); n != 1 {
		t.Fatalf("waiting to merge after the branch merged: %d, want 1", n)
	}
	// The run records the merge commit.
	runs := root.call(t, "amp_get_task", map[string]any{"task_id": float64(onBranch[0]), "include": []any{"runs"}})
	if !strings.Contains(fmt.Sprint(runs), "fedcba9876543") {
		t.Fatalf("merge commit not recorded: %v", runs)
	}

	// By PR URL (REST's "pr" name works too) and by commit.
	if out, isErr := tokenToolCall(t, url, carolTok, "amp_report_merged", map[string]any{"pr": pr}); isErr || out["count"] != 1.0 {
		t.Fatalf("CI merge by PR: %v", out)
	}
	if out, isErr := tokenToolCall(t, url, carolTok, "amp_report_merged", map[string]any{"commit": sha}); isErr || out["count"] != 1.0 {
		t.Fatalf("CI merge by commit: %v", out)
	}
	if n := unmerged(); n != 0 {
		t.Fatalf("waiting to merge after CI: %d, want 0", n)
	}

	// Someone outside the project marks nothing.
	outsiderTok := mintToken(t, ra, "outsider", "me")
	if out, isErr := tokenToolCall(t, url, outsiderTok, "amp_report_merged", map[string]any{"branch": branch}); isErr || out["count"] != 0.0 {
		t.Fatalf("outsider merge report: %v", out)
	}
	// Nothing to match on is refused; so is a bad token.
	if out, isErr := tokenToolCall(t, url, carolTok, "amp_report_merged", map[string]any{}); !isErr {
		t.Fatalf("no commit/branch/PR accepted: %v", out)
	}
	h := modernHeaders("tools/call", "amp_report_merged")
	h["Authorization"] = "Bearer amp_not-a-real-token"
	if r := post(t, url, h, modernToolCall(1, "amp_report_merged", map[string]any{"branch": branch})); r.status != 401 {
		t.Fatalf("bad token: HTTP %d %s", r.status, r.raw)
	}
}
