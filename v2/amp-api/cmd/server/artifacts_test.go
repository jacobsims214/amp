package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/service"
)

const mockupHTML = `<!doctype html><title>Settings</title><h1>Settings</h1><script>document.title="ok"</script>`

// The whole loop: an agent publishes a mockup to its person's ticket and asks
// for review; a person asks for changes (which answers the ticket's
// question), the agent publishes v2, and the person approves it. Content is
// served sandboxed; access follows the project and the ticket.
func TestArtifacts_ReviewLoop(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice) // Devs member; owns the design ticket
	design := mkIn(t, alice, w.s.storyID, "Mockup: settings page")
	build := mkIn(t, alice, w.s.storyID, "Build settings page", design)

	out := alice.call(t, "amp_publish_artifact", map[string]any{
		"task_id": float64(design), "title": "Settings mockup", "kind": "html", "content": mockupHTML, "note": "first pass",
	})
	art := out["artifact"].(map[string]any)
	id := intField(t, art, "id")
	if art["status"] != "in_review" || intField(t, art, "current_version") != 1 || art["kind"] != "html" || !strings.Contains(out["next"].(string), "amp_block_task") {
		t.Fatalf("published: %v", out)
	}
	if !strings.Contains(lastComment(t, alice, design), fmt.Sprintf("Published artifact #%d", id)) {
		t.Fatal("publishing should be noted on the ticket")
	}

	// Anyone on the project can see it; people outside it can't.
	list := w.as(w.carol).call(t, "amp_list_artifacts", map[string]any{"task_id": float64(design)})["artifacts"].([]any)
	if len(list) != 1 {
		t.Fatalf("carol's list: %v", list)
	}
	wantErr(t, w.as(w.outsider).callExpectError(t, "amp_get_artifact", map[string]any{"artifact_id": float64(id)}), "not found")
	ra.want(t, 404, "outsider", http.MethodGet, fmt.Sprintf("/api/artifacts/%d", id), nil)
	// Only someone who can work the ticket can publish to it.
	if msg := w.as(w.carol).callExpectError(t, "amp_publish_artifact", map[string]any{
		"task_id": float64(design), "title": "x", "kind": "html", "content": "<p>x</p>",
	}); msg == "" {
		t.Fatal("carol published to alice's ticket")
	}

	// The content is served as-is, sandboxed.
	resp, body := rawGet(t, ra, "bob", fmt.Sprintf("/api/artifacts/%d/content", id))
	if resp.StatusCode != 200 || body != mockupHTML {
		t.Fatalf("content: %d %q", resp.StatusCode, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "sandbox ") || strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("CSP %q must sandbox without allow-same-origin", csp)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("headers: %v", resp.Header)
	}

	// The agent asks for review and stops.
	alice.call(t, "amp_block_task", map[string]any{"task_id": float64(design), "reason": fmt.Sprintf("Review artifact #%d?", id)})

	// Reviewing takes the ticket's owner or a lead (not any member), and changes need a reason.
	path := fmt.Sprintf("/api/artifacts/%d/review", id)
	ra.want(t, 403, "bob", http.MethodPost, path, map[string]any{"approve": true})
	ra.want(t, 400, "alice", http.MethodPost, path, map[string]any{"approve": false})
	got := ra.want(t, 200, "alice", http.MethodPost, path, map[string]any{"approve": false, "note": "Put Save at the bottom."})["artifact"].(map[string]any)
	if got["status"] != "changes_requested" || got["approved_version"] != nil {
		t.Fatalf("after changes requested: %v", got)
	}
	// The verdict answered the question: the ticket is ready again, with the feedback on it.
	if task := getTask(t, alice, design); task["state"] != "backlog" {
		t.Fatalf("design ticket after review: %v", task["state"])
	}
	if c := lastComment(t, alice, design); !strings.Contains(c, "Changes requested") || !strings.Contains(c, "Put Save at the bottom.") {
		t.Fatalf("feedback comment: %q", c)
	}

	// v2 goes back into review; a stale version can't be approved.
	v2 := alice.call(t, "amp_publish_artifact", map[string]any{
		"artifact_id": float64(id), "content": strings.Replace(mockupHTML, "</h1>", "</h1><button>Save</button>", 1), "note": "Save at the bottom",
	})["artifact"].(map[string]any)
	if v2["status"] != "in_review" || intField(t, v2, "current_version") != 2 {
		t.Fatalf("v2: %v", v2)
	}
	ra.want(t, 400, "alice", http.MethodPost, path, map[string]any{"approve": true, "version": 1})
	// A project owner can review too (no open question now: it's just noted).
	ok := ra.want(t, 200, "owner", http.MethodPost, path, map[string]any{"approve": true})["artifact"].(map[string]any)
	if ok["status"] != "approved" || intField(t, ok, "approved_version") != 2 {
		t.Fatalf("approved: %v", ok)
	}
	if c := lastComment(t, alice, design); !strings.Contains(c, fmt.Sprintf("Approved artifact #%d", id)) {
		t.Fatalf("approval comment: %q", c)
	}

	// From chat: the manager records the user's verdict (here, asking for one more change).
	again := alice.call(t, "amp_review_artifact", map[string]any{"artifact_id": float64(id), "approve": false, "note": "Bigger heading."})["artifact"].(map[string]any)
	if again["status"] != "changes_requested" || intField(t, again, "approved_version") != 2 {
		t.Fatalf("chat review: %v", again)
	}
	wantErr(t, alice.callExpectError(t, "amp_review_artifact", map[string]any{"artifact_id": float64(id), "approve": false}), "what to change")

	// The builder reads what was approved, and the first version is still there.
	g := w.as(w.lead).call(t, "amp_get_artifact", map[string]any{"artifact_id": float64(id), "version": float64(2)})
	if !strings.Contains(g["content"].(string), "<button>Save</button>") || len(g["versions"].([]any)) != 2 {
		t.Fatalf("get v2: %v", g)
	}
	if g1 := alice.call(t, "amp_get_artifact", map[string]any{"artifact_id": float64(id), "version": float64(1)}); g1["content"] != mockupHTML {
		t.Fatalf("v1 content: %v", g1["content"])
	}
	_ = build
}

// Kinds are checked against content; images reach agents as images; agents
// can't review; and a project-level artifact needs no ticket.
func TestArtifacts_KindsAndRules(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	pid := float64(w.s.projectID)

	wantErr(t, alice.callExpectError(t, "amp_publish_artifact", map[string]any{"project_id": pid, "title": "x", "kind": "svg", "content": "<p>no</p>"}), "svg")
	wantErr(t, alice.callExpectError(t, "amp_publish_artifact", map[string]any{"project_id": pid, "title": "x", "kind": "image", "content": "<p>no</p>"}), "image")
	wantErr(t, alice.callExpectError(t, "amp_publish_artifact", map[string]any{"project_id": pid, "title": "x", "kind": "video", "content": "x"}), "kind must be")
	wantErr(t, alice.callExpectError(t, "amp_publish_artifact", map[string]any{"project_id": pid, "kind": "text", "content": "x"}), "title")
	wantErr(t, alice.callExpectError(t, "amp_publish_artifact", map[string]any{"project_id": pid, "title": "x", "kind": "text"}), "empty")

	// A 1×1 PNG, as an image.
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	img := alice.call(t, "amp_publish_artifact", map[string]any{"project_id": pid, "title": "Dot", "kind": "image",
		"content_base64": base64.StdEncoding.EncodeToString(png)})["artifact"].(map[string]any)
	if img["task_id"] != nil {
		t.Fatalf("project-level artifact: %v", img)
	}
	contents := rawToolContent(t, alice, "amp_get_artifact", map[string]any{"artifact_id": float64(intField(t, img, "id"))})
	if len(contents) != 2 || contents[1]["type"] != "image" || contents[1]["mimeType"] != "image/png" {
		t.Fatalf("image result content: %v", contents)
	}
	resp, _ := rawGet(t, ra, "carol", fmt.Sprintf("/api/artifacts/%d/content", intField(t, img, "id")))
	if resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("image content-type %q", resp.Header.Get("Content-Type"))
	}

	// Agent accounts can't review, even with access to the project.
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	agent := authz.Principal{UserID: w.alice.User.ID, Email: "bot@agents.amp", Agent: true}
	if _, err := svc.ReviewArtifact(context.Background(), agent, intField(t, img, "id"), 0, true, ""); authz.StatusOf(err, 0) != 403 {
		t.Fatalf("agent review: %v", err)
	}

	// Over REST (scripts, CI): publish, list.
	pub := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/projects/%d/artifacts", w.s.projectID),
		map[string]any{"title": "Flow", "kind": "svg", "content": `<svg xmlns="http://www.w3.org/2000/svg"/>`})["artifact"].(map[string]any)
	if pub["kind"] != "svg" {
		t.Fatalf("rest publish: %v", pub)
	}
	// dave is in the org but not on the project.
	ra.want(t, 404, "dave", http.MethodGet, fmt.Sprintf("/api/projects/%d/artifacts", w.s.projectID), nil)
	if n := ra.want(t, 200, "bob", http.MethodGet, fmt.Sprintf("/api/projects/%d/artifacts", w.s.projectID), nil)["count"]; n != float64(2) {
		t.Fatalf("bob's list count %v", n)
	}
}

func rawGet(t *testing.T, ra *restAs, who, path string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ra.server.URL+path, nil)
	req.Header.Set("X-Test-As", who)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// rawToolContent calls a tool and returns its content items as maps.
func rawToolContent(t *testing.T, s *mcpStack, tool string, args map[string]any) []map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	b, _ := json.Marshal(s.srv.HandleMessage(s.callCtx(), raw))
	var resp struct {
		Result struct {
			Content []map[string]any `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Result.Content
}

func lastComment(t *testing.T, s *mcpStack, taskID int) string {
	t.Helper()
	cs := s.call(t, "amp_get_task_comments", map[string]any{"task_id": float64(taskID)})["comments"].([]any)
	if len(cs) == 0 {
		return ""
	}
	return cs[len(cs)-1].(map[string]any)["body"].(string)
}

func getTask(t *testing.T, s *mcpStack, id int) map[string]any {
	t.Helper()
	out := s.call(t, "amp_get_task", map[string]any{"task_id": float64(id)})
	if task, ok := out["task"].(map[string]any); ok {
		return task
	}
	return out
}

// Retention: artifacts expire unless a person keeps them; their creator and
// the ticket's owner are warned first and can keep them or let them go.
func TestArtifacts_Retention(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	ctx := context.Background()
	db, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	expireIn := func(id int, d time.Duration) {
		if _, err := db.Exec(ctx, `UPDATE artifacts SET expires_at = $2 WHERE id = $1`, id, time.Now().Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	ticket := mkIn(t, alice, w.s.storyID, "Mockup: profile")
	pub := func(title string) int {
		a := alice.call(t, "amp_publish_artifact", map[string]any{"task_id": float64(ticket), "title": title, "kind": "text", "content": title})
		return intField(t, a["artifact"].(map[string]any), "id")
	}
	soon, kept := pub("goes soon"), pub("kept")

	a := ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/artifacts/%d", soon), nil)["artifact"].(map[string]any)
	exp, _ := time.Parse(time.RFC3339Nano, a["expires_at"].(string))
	if d := time.Until(exp); d < 44*24*time.Hour || d > 46*24*time.Hour || a["keep"] != false {
		t.Fatalf("new artifact expires in %v, keep=%v; want ~45 days", d, a["keep"])
	}
	expiring := func(who string, days int) []any {
		return ra.want(t, 200, who, http.MethodGet, fmt.Sprintf("/api/artifacts/expiring?days=%d", days), nil)["artifacts"].([]any)
	}
	if n := len(expiring("alice", 14)); n != 0 {
		t.Fatalf("nothing expires within 14 days yet, got %d", n)
	}

	expireIn(soon, 3*24*time.Hour)
	expireIn(kept, 3*24*time.Hour)
	if got := expiring("alice", 14); len(got) != 2 || intField(t, got[0].(map[string]any), "id") != soon {
		t.Fatalf("alice's warnings: %v", got)
	}
	if n := len(expiring("bob", 14)); n != 0 {
		t.Fatalf("bob neither made them nor owns the ticket, but is warned about %d", n)
	}

	// Keep one (a person; agent accounts can't); let the other go.
	k := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/artifacts/%d/keep", kept), map[string]any{"keep": true})["artifact"].(map[string]any)
	if k["keep"] != true || k["kept_by"] == nil {
		t.Fatalf("kept: %v", k)
	}
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	if _, err := svc.KeepArtifact(ctx, authz.Principal{UserID: w.alice.User.ID, Agent: true}, soon, true); authz.StatusOf(err, 0) != 403 {
		t.Fatalf("agent keep: %v", err)
	}
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/artifacts/%d/let-expire", soon), nil)
	if n := len(expiring("alice", 14)); n != 0 {
		t.Fatalf("after keep and let-expire, still warned about %d", n)
	}

	// Past expiry: the purge deletes the unkept one, notes it on the ticket, and spares the kept one.
	expireIn(soon, -time.Minute)
	expireIn(kept, -time.Minute)
	if _, err := svc.PurgeExpiredArtifacts(ctx); err != nil {
		t.Fatal(err)
	}
	ra.want(t, 404, "alice", http.MethodGet, fmt.Sprintf("/api/artifacts/%d", soon), nil)
	ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/artifacts/%d", kept), nil)
	if c := lastComment(t, alice, ticket); !strings.Contains(c, fmt.Sprintf("Artifact #%d", soon)) || !strings.Contains(c, "expired") {
		t.Fatalf("purge comment: %q", c)
	}

	// A new version pushes expiry back out and clears a dismissal.
	other := pub("revised")
	expireIn(other, time.Hour)
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/artifacts/%d/let-expire", other), nil)
	v2 := alice.call(t, "amp_publish_artifact", map[string]any{"artifact_id": float64(other), "content": "v2"})["artifact"].(map[string]any)
	exp2, _ := time.Parse(time.RFC3339Nano, v2["expires_at"].(string))
	if time.Until(exp2) < 44*24*time.Hour || v2["expiry_dismissed_at"] != nil {
		t.Fatalf("v2 expiry %v dismissed %v", exp2, v2["expiry_dismissed_at"])
	}
	// Unkeeping gives a full retention period again, from now.
	un := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/artifacts/%d/keep", kept), map[string]any{"keep": false})["artifact"].(map[string]any)
	exp3, _ := time.Parse(time.RFC3339Nano, un["expires_at"].(string))
	if un["keep"] != false || time.Until(exp3) < 44*24*time.Hour {
		t.Fatalf("unkept: %v", un)
	}
}

func newSvc(w *rbacWorld) *service.Service { return service.New(w.s.repo, w.s.reg, w.s.kb) }

func expireNow(t *testing.T, w *rbacWorld, id int) error {
	t.Helper()
	db, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close(context.Background())
	_, err = db.Exec(context.Background(), `UPDATE artifacts SET expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, id)
	return err
}
