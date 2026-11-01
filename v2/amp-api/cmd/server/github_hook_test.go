package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// Inbound GitHub merge webhook (AMP #2479, KB decisions/human-work-ux.md §4).

const testRepo = "acme/app"

// ghHook is a project's webhook under test: its secret and a client.
type ghHook struct {
	ra        *restAs
	projectID int
	secret    string
	n         int
}

// setUpGitHubHook generates the project's secret as its owner.
func setUpGitHubHook(t *testing.T, ra *restAs, projectID int) *ghHook {
	t.Helper()
	out := ra.want(t, 200, "owner", http.MethodPost, fmt.Sprintf("/api/projects/%d/github-hook/secret", projectID), nil)
	secret, _ := out["secret"].(string)
	if len(secret) < 32 || out["configured"] != true || !strings.HasSuffix(fmt.Sprint(out["url"]), fmt.Sprintf("/api/hooks/github/%d", projectID)) {
		t.Fatalf("generate secret = %v", out)
	}
	return &ghHook{ra: ra, projectID: projectID, secret: secret}
}

// post sends a delivery. sig: "" signs with the hook's secret, "-" sends
// none, anything else is sent as is. id: "" makes a new delivery id.
func (h *ghHook) post(t *testing.T, event, id, sig string, payload any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/hooks/github/%d", h.ra.server.URL, h.projectID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	if id == "" {
		h.n++
		id = uniq(fmt.Sprintf("delivery-%d", h.n))
	}
	req.Header.Set("X-GitHub-Delivery", id)
	switch sig {
	case "":
		req.Header.Set("X-Hub-Signature-256", service.SignGitHubPayload(h.secret, body))
	case "-":
	default:
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (h *ghHook) want(t *testing.T, code int, event, id, sig string, payload any) map[string]any {
	t.Helper()
	got, out := h.post(t, event, id, sig, payload)
	if got != code {
		t.Fatalf("delivery %s: %d %v, want %d", event, got, out, code)
	}
	return out
}

// prClosed is a pull_request closed payload.
func prClosed(merged bool, url, headRepo, branch, headSHA, mergeSHA string) map[string]any {
	return map[string]any{
		"action": "closed",
		"number": 7,
		"pull_request": map[string]any{
			"html_url":         url,
			"merged":           merged,
			"merge_commit_sha": mergeSHA,
			"head":             map[string]any{"ref": branch, "sha": headSHA, "repo": map[string]any{"full_name": headRepo}},
			"base":             map[string]any{"ref": "main", "sha": "0000000", "repo": map[string]any{"full_name": testRepo}},
		},
		"repository": map[string]any{"full_name": testRepo},
	}
}

func sha(seed string) string {
	sum := sha1.Sum([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func TestGitHubHook_SecretIsOwnersOnlyAndShownOnce(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	path := fmt.Sprintf("/api/projects/%d/github-hook", w.s.projectID)

	off := ra.want(t, 200, "owner", http.MethodGet, path, nil)
	if off["configured"] != false {
		t.Fatalf("before setup = %v", off)
	}
	for _, who := range []string{"alice", "lead"} {
		ra.want(t, 403, who, http.MethodGet, path, nil)
		ra.want(t, 403, who, http.MethodPost, path+"/secret", nil)
		ra.want(t, 403, who, http.MethodDelete, path, nil)
	}
	h := setUpGitHubHook(t, ra, w.s.projectID)
	got := ra.want(t, 200, "owner", http.MethodGet, path, nil)
	if got["configured"] != true || got["secret"] != nil || strings.Contains(mustJSON(got), h.secret) {
		t.Fatalf("GET must not return the secret: %v", got)
	}
	// The stored value isn't the secret in the clear when a key is set;
	// either way the API never returns it again.
	ping := h.want(t, 200, "ping", "", "", map[string]any{"zen": "hi"})
	if !strings.Contains(fmt.Sprint(ping["result"]), "pong") {
		t.Fatalf("ping = %v", ping)
	}

	// Rotating: the old secret stops working.
	old := h.secret
	rot := ra.want(t, 200, "owner", http.MethodPost, path+"/secret", nil)
	if rot["secret"] == old || rot["secret"] == nil {
		t.Fatalf("rotate = %v", rot)
	}
	h.want(t, 401, "ping", "", "", map[string]any{})
	h.secret = rot["secret"].(string)
	h.want(t, 200, "ping", "", "", map[string]any{})

	// The delivery log shows signed deliveries only; rejections are counted.
	got = ra.want(t, 200, "owner", http.MethodGet, path, nil)
	if n := len(got["deliveries"].([]any)); n != 2 {
		t.Fatalf("deliveries logged = %d, want 2 (pings): %v", n, got["deliveries"])
	}
	if hook := got["hook"].(map[string]any); hook["rejected_count"] != float64(1) {
		t.Fatalf("rejected_count = %v", hook["rejected_count"])
	}

	// Turned off: deliveries are refused.
	ra.want(t, 200, "owner", http.MethodDelete, path, nil)
	h.want(t, 404, "ping", "", "", map[string]any{})
}

// A merged PR (signed) records the merge from GitHub and releases the
// dependent; a bad or missing signature changes nothing; a replay is a
// no-op; other events are ignored.
func TestGitHubHook_MergedPRRecordsMerge(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	build, deploy := mergeChain(t, w, "gh")
	pr := "https://github.com/" + testRepo + "/pull/" + uniq("7")
	finishWith(t, alice, build, map[string]any{"pr_url": pr, "pushed": true})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNeeded {
		t.Fatalf("build merge_state = %q", ms)
	}
	h := setUpGitHubHook(t, ra, w.s.projectID)
	payload := prClosed(true, pr, testRepo, uniq("unrelated"), sha("head-gh"), sha("merge-gh"))

	// Bad and missing signatures: 401, nothing changes.
	h.want(t, 401, "pull_request", "", "sha256="+strings.Repeat("ab", 32), payload)
	h.want(t, 401, "pull_request", "", "-", payload)
	h.want(t, 401, "pull_request", "", "sha1=deadbeef", payload)
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNeeded {
		t.Fatalf("after bad signatures merge_state = %q", ms)
	}
	if st := stateOf(t, alice, deploy); st != "blocked" {
		t.Fatalf("deploy after bad signatures = %s", st)
	}

	// Ignored events.
	h.want(t, 204, "issues", "", "", map[string]any{"action": "opened"})
	opened := prClosed(false, pr, testRepo, "x", "", "")
	opened["action"] = "opened"
	h.want(t, 204, "pull_request", "", "", opened)

	// Valid: merged, from GitHub.
	id := uniq("merged-delivery")
	out := h.want(t, 200, "pull_request", id, "", payload)
	if fmt.Sprint(out["task_ids"]) != fmt.Sprintf("[%d]", build) {
		t.Fatalf("merged delivery = %v", out)
	}
	b := planTask(t, alice, build)
	if b["merge_state"] != domain.MergeMerged || b["merge_source"] != domain.MergeSourceGitHub {
		t.Fatalf("build after the webhook = %v", b)
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after the merge = %s, want backlog", st)
	}
	runs := len(alice.call(t, "amp_get_task", map[string]any{"task_id": float64(build), "include": []any{"runs"}})["runs"].([]any))

	// Replay: same delivery id, a no-op.
	again := h.want(t, 200, "pull_request", id, "", payload)
	if again["replayed"] != true {
		t.Fatalf("replay = %v", again)
	}
	// A new delivery of the same event: already merged, recorded nothing.
	dup := h.want(t, 200, "pull_request", "", "", payload)
	if dup["task_ids"] != nil || !strings.Contains(fmt.Sprint(dup["result"]), "already recorded") {
		t.Fatalf("second delivery = %v", dup)
	}
	if n := len(alice.call(t, "amp_get_task", map[string]any{"task_id": float64(build), "include": []any{"runs"}})["runs"].([]any)); n != runs {
		t.Fatalf("replays added runs: %d -> %d", runs, n)
	}
	// A replay never changes a closed-unmerged ticket either: abandoned
	// only applies to tickets waiting on their merge.
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeMerged {
		t.Fatalf("merge_state after replays = %q", ms)
	}
}

func TestGitHubHook_ClosedUnmergedAbandons(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	build, deploy := mergeChain(t, w, "gh-abandon")
	pr := "https://github.com/" + testRepo + "/pull/" + uniq("8")
	finishWith(t, alice, build, map[string]any{"pr_url": pr, "pushed": true})
	h := setUpGitHubHook(t, ra, w.s.projectID)

	out := h.want(t, 200, "pull_request", "", "", prClosed(false, pr, testRepo, "", "", ""))
	if fmt.Sprint(out["task_ids"]) != fmt.Sprintf("[%d]", build) || !strings.Contains(fmt.Sprint(out["result"]), "abandoned") {
		t.Fatalf("closed delivery = %v", out)
	}
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeAbandoned {
		t.Fatalf("build merge_state = %q, want abandoned", ms)
	}
	if st := stateOf(t, alice, deploy); st != "blocked" {
		t.Fatalf("deploy after abandon = %s, want blocked", st)
	}
}

// Matching by head branch (same repository only) and by commit (head SHA,
// short or full, and the merge commit), limited to the hook's project.
func TestGitHubHook_BranchAndCommitMatching(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	h := setUpGitHubHook(t, ra, w.s.projectID)

	// Branch: matched when the PR's head is in the base repository.
	byBranch, _ := mergeChain(t, w, "gh-branch")
	branch := uniq("feat/gh")
	finishWith(t, alice, byBranch, map[string]any{"branch": branch, "pushed": true})
	// A fork's PR with the same branch name doesn't match.
	h.want(t, 200, "pull_request", "", "", prClosed(true, "https://github.com/fork/app/pull/1", "someone/app", branch, sha("fork"), sha("fork-merge")))
	if ms := mergeStateOf(t, alice, byBranch); ms != domain.MergeNeeded {
		t.Fatalf("a fork's branch matched: merge_state %q", ms)
	}
	h.want(t, 200, "pull_request", "", "", prClosed(true, "https://github.com/"+testRepo+"/pull/"+uniq("b"), testRepo, branch, sha("b-head"), sha("b-merge")))
	if b := planTask(t, alice, byBranch); b["merge_state"] != domain.MergeMerged || b["merge_source"] != domain.MergeSourceGitHub {
		t.Fatalf("by branch = %v", b)
	}

	// Head commit: the run reported a short SHA, GitHub sends the full one.
	byHead, _ := mergeChain(t, w, "gh-head")
	head := sha("head-commit-" + uniq("h"))
	finishWith(t, alice, byHead, map[string]any{"commit": head[:9], "branch": uniq("other"), "pushed": true})
	h.want(t, 200, "pull_request", "", "", prClosed(true, "https://github.com/"+testRepo+"/pull/"+uniq("h"), "someone/app", "x", head, sha("h-merge")))
	if ms := mergeStateOf(t, alice, byHead); ms != domain.MergeMerged {
		t.Fatalf("by head commit: merge_state %q", ms)
	}

	// Merge commit: the run reported the commit that landed.
	byMerge, _ := mergeChain(t, w, "gh-merge")
	merge := sha("merge-commit-" + uniq("m"))
	finishWith(t, alice, byMerge, map[string]any{"commit": merge, "branch": uniq("another"), "pushed": true})
	h.want(t, 200, "pull_request", "", "", prClosed(true, "https://github.com/"+testRepo+"/pull/"+uniq("m"), "someone/app", "y", sha("m-head"), merge))
	if ms := mergeStateOf(t, alice, byMerge); ms != domain.MergeMerged {
		t.Fatalf("by merge commit: merge_state %q", ms)
	}

	// Another project's ticket on the same branch is never touched.
	w2 := newRBACWorld(t)
	alice2 := w2.s.as(w2.alice)
	theirs, _ := mergeChain(t, w2, "gh-elsewhere")
	shared := uniq("feat/shared")
	finishWith(t, alice2, theirs, map[string]any{"branch": shared, "pushed": true})
	out := h.want(t, 200, "pull_request", "", "", prClosed(true, "https://github.com/"+testRepo+"/pull/"+uniq("s"), testRepo, shared, sha("s-head"), sha("s-merge")))
	if !strings.Contains(fmt.Sprint(out["result"]), "no ticket in this project") {
		t.Fatalf("delivery for another project's branch = %v", out)
	}
	if ms := mergeStateOf(t, alice2, theirs); ms != domain.MergeNeeded {
		t.Fatalf("another project's ticket changed: merge_state %q", ms)
	}
}

func TestGitHubHook_LimitsAndUnknownProject(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	h := setUpGitHubHook(t, ra, w.s.projectID)

	// Too large: 413 before anything is read.
	big := map[string]any{"pad": strings.Repeat("x", 3<<20)}
	if code, _ := h.post(t, "ping", "", "", big); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body = %d, want 413", code)
	}
	// GET isn't served.
	resp, err := http.Get(fmt.Sprintf("%s/api/hooks/github/%d", ra.server.URL, w.s.projectID))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", resp.StatusCode)
	}
	// A project without a hook, and one that doesn't exist.
	none := &ghHook{ra: ra, projectID: 1 << 30, secret: h.secret}
	none.want(t, 404, "ping", "", "", map[string]any{})
	// A signed delivery without an id is refused.
	if code, _ := h.post(t, "ping", " ", "", map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("no delivery id = %d, want 400", code)
	}
}

// The hook needs no AMP session: main's auth middleware lets it through
// (and only it) while every other /api route stays 401 without one.
func TestGitHubHook_BypassesSessionAuthOnly(t *testing.T) {
	w := newRBACWorld(t)
	idp := newTestIdP(t)
	v, err := auth.NewVerifier(context.Background(), auth.Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL, Repo: w.s.repo})
	if err != nil {
		t.Fatal(err)
	}
	ra := newRestAs(t, w)
	h := setUpGitHubHook(t, ra, w.s.projectID)
	handler := authExceptHealth(v, apiMux(api.NewRestHandler(w.s.reg, w.s.repo, w.s.hub, w.s.kb)))
	do := func(path string, signed bool) int {
		body := []byte(`{"zen":"x"}`)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "ping")
		req.Header.Set("X-GitHub-Delivery", uniq("bypass"))
		if signed {
			req.Header.Set("X-Hub-Signature-256", service.SignGitHubPayload(h.secret, body))
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	hook := fmt.Sprintf("/api/hooks/github/%d", w.s.projectID)
	if got := do(hook, true); got != http.StatusOK {
		t.Fatalf("signed delivery without a session = %d, want 200", got)
	}
	if got := do(hook, false); got != http.StatusUnauthorized {
		t.Fatalf("unsigned delivery = %d, want 401", got)
	}
	if got := do(fmt.Sprintf("/api/projects/%d/github-hook/secret", w.s.projectID), true); got != http.StatusUnauthorized {
		t.Fatalf("settings route without a session = %d, want 401", got)
	}
}
