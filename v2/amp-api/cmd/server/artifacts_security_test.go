package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/repository"
)

// Artifacts are only for people on the project: everyone else gets "not
// found" for every route and tool, including publishing onto its tickets.
func TestArtifactsSecurity_Access(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	ticket := mkIn(t, alice, w.s.storyID, "Mockup: secret screen")
	id := intField(t, alice.call(t, "amp_publish_artifact", map[string]any{"task_id": float64(ticket), "title": "Secret",
		"kind": "html", "content": "<p>secret</p>"})["artifact"].(map[string]any), "id")

	for _, who := range []string{"outsider", "dave"} { // another org; this org but not on the project
		for _, req := range []struct{ method, path string }{
			{"GET", fmt.Sprintf("/api/artifacts/%d", id)},
			{"GET", fmt.Sprintf("/api/artifacts/%d/content", id)},
			{"GET", fmt.Sprintf("/api/artifacts/%d/activity", id)},
			{"GET", fmt.Sprintf("/api/projects/%d/artifacts", w.s.projectID)},
		} {
			ra.want(t, 404, who, req.method, req.path, nil)
		}
		ra.want(t, 404, who, "POST", fmt.Sprintf("/api/artifacts/%d/review", id), map[string]any{"approve": true})
		ra.want(t, 404, who, "POST", fmt.Sprintf("/api/artifacts/%d/keep", id), map[string]any{"keep": true})
		ra.want(t, 404, who, "POST", fmt.Sprintf("/api/artifacts/%d/versions", id), map[string]any{"content": "<p>x</p>"})
	}
	ra.want(t, 401, "", "GET", fmt.Sprintf("/api/artifacts/%d/content", id), nil)
	out := w.as(w.outsider)
	for _, tool := range []map[string]any{
		{"name": "amp_get_artifact", "args": map[string]any{"artifact_id": float64(id)}},
		{"name": "amp_list_artifacts", "args": map[string]any{"task_id": float64(ticket)}},
		{"name": "amp_publish_artifact", "args": map[string]any{"task_id": float64(ticket), "title": "x", "kind": "text", "content": "x"}},
		{"name": "amp_publish_artifact", "args": map[string]any{"artifact_id": float64(id), "content": "x"}},
		{"name": "amp_review_artifact", "args": map[string]any{"artifact_id": float64(id), "approve": true}},
		{"name": "amp_keep_artifact", "args": map[string]any{"artifact_id": float64(id), "keep": true}},
	} {
		if msg := out.callExpectError(t, tool["name"].(string), tool["args"].(map[string]any)); !strings.Contains(msg, "not found") {
			t.Errorf("outsider %s: %q (want not found)", tool["name"], msg)
		}
	}

	// The sandbox: scripts run, but no same origin, no top navigation, no downloads.
	resp, _ := rawGet(t, ra, "bob", fmt.Sprintf("/api/artifacts/%d/content", id))
	csp := resp.Header.Get("Content-Security-Policy")
	for _, bad := range []string{"allow-same-origin", "allow-top-navigation", "allow-downloads", "allow-popups-to-escape-sandbox"} {
		if strings.Contains(csp, bad) {
			t.Errorf("CSP %q allows %s", csp, bad)
		}
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(resp.Header.Get("Cache-Control"), "private") {
		t.Errorf("headers: %v", resp.Header)
	}
}

// A sandboxed artifact's scripts send Origin: null; browsers mark other
// sites' requests cross-site. Neither can change anything.
func TestArtifactsSecurity_CrossSiteWrites(t *testing.T) {
	reached := false
	h := api.RejectCrossSiteWrites(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	for _, c := range []struct {
		method, origin, site string
		ok                   bool
	}{
		{"POST", "null", "", false},
		{"POST", "", "cross-site", false},
		{"DELETE", "null", "", false},
		{"POST", "https://amp.example.com", "same-origin", true},
		{"POST", "", "", true},              // CLIs and agents
		{"GET", "null", "cross-site", true}, // reads are protected by auth and CORS instead
	} {
		reached = false
		req := httptest.NewRequest(c.method, "/api/artifacts/1/review", strings.NewReader(`{"approve":true}`))
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.site != "" {
			req.Header.Set("Sec-Fetch-Site", c.site)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if reached != c.ok || (!c.ok && rec.Code != 403) {
			t.Errorf("%+v: reached=%v code=%d", c, reached, rec.Code)
		}
	}
}

// Publishing, reviewing and viewing are audited with who and when; views are
// de-duplicated per person. The creator, the ticket owner and leads can read
// an artifact's activity; other members can't see who looked.
func TestArtifactsSecurity_Audit(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.as(w.alice)
	ticket := mkIn(t, alice, w.s.storyID, "Mockup: audited")
	id := intField(t, alice.call(t, "amp_publish_artifact", map[string]any{"task_id": float64(ticket), "title": "Audited",
		"kind": "text", "content": "hello"})["artifact"].(map[string]any), "id")
	path := fmt.Sprintf("/api/artifacts/%d", id)
	ra.want(t, 200, "bob", "GET", path, nil)
	ra.want(t, 200, "bob", "GET", path, nil) // reload: same view
	w.as(w.carol).call(t, "amp_get_artifact", map[string]any{"artifact_id": float64(id)})
	ra.want(t, 200, "alice", "POST", path+"/review", map[string]any{"approve": true})

	ra.want(t, 403, "bob", "GET", path+"/activity", nil) // a member, not creator/owner/lead
	act := ra.want(t, 200, "lead", "GET", path+"/activity", nil)["activity"].([]any)
	count := map[string]int{}
	viewers := map[string]bool{}
	for _, e := range act {
		m := e.(map[string]any)
		count[m["action"].(string)]++
		if m["action"] == "artifact.view" {
			viewers[m["actor_email"].(string)] = true
		}
	}
	if count["artifact.publish"] != 1 || count["artifact.approve"] != 1 || count["artifact.view"] != 2 ||
		!viewers[w.bob.User.Email] || !viewers[w.carol.User.Email] {
		t.Fatalf("activity: %v viewers %v", count, viewers)
	}
	ra.want(t, 200, "alice", "GET", path+"/activity", nil) // the creator

	// Expiry is audited too.
	repo := w.s.repo
	svc := newSvc(w)
	if err := expireNow(t, w, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PurgeExpiredArtifacts(context.Background()); err != nil {
		t.Fatal(err)
	}
	trail, _ := repo.TargetAudit(context.Background(), "artifact", fmt.Sprint(id), 50)
	if len(trail) == 0 || trail[0].Action != "artifact.expire" {
		t.Fatalf("purge not audited: %v", trail)
	}
	_ = repository.ExpiredArtifact{}
}

// One artifact keeps at most 100 versions.
func TestArtifactsSecurity_VersionCap(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.as(w.alice)
	id := intField(t, alice.call(t, "amp_publish_artifact", map[string]any{"project_id": float64(w.s.projectID), "title": "Many",
		"kind": "text", "content": "v1"})["artifact"].(map[string]any), "id")
	for i := 2; i <= 100; i++ {
		alice.call(t, "amp_publish_artifact", map[string]any{"artifact_id": float64(id), "content": fmt.Sprint("v", i)})
	}
	wantErr(t, alice.callExpectError(t, "amp_publish_artifact", map[string]any{"artifact_id": float64(id), "content": "v101"}), "100 versions")
}
