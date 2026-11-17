package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/service"
)

// A worker publishes a ~2 MB local HTML file to its ticket, then as v2,
// over MCP plus a sessionless PUT through main's auth middleware: the
// Claude Code route, where no AMP token is on disk (AMP #4937).
func TestArtifactUpload_MCPThenSessionlessPut(t *testing.T) {
	w := newRBACWorld(t)
	idp := newTestIdP(t)
	v, err := auth.NewVerifier(context.Background(), auth.Config{IssuerURL: testIssuer, DiscoveryURL: idp.server.URL, Repo: w.s.repo})
	if err != nil {
		t.Fatal(err)
	}
	handler := authExceptHealth(v, apiMux(api.NewRestHandler(w.s.reg, w.s.repo, w.s.hub, w.s.kb)))
	put := func(url string, body []byte) (int, map[string]any) {
		t.Helper()
		if i := strings.Index(url, service.ArtifactUploadPath); i >= 0 {
			url = url[i:]
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, url, bytes.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	page := func(tag string) []byte {
		b := []byte("<!doctype html><html><body><h1>" + tag + "</h1><img src=\"data:image/png;base64,")
		b = append(b, bytes.Repeat([]byte("iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"), (2<<20)/32)...)
		return append(b, "\"></body></html>"...)
	}
	alice := w.as(w.alice)
	task := mkIn(t, alice, w.s.storyID, "screenshot mockup")
	mint := func(args map[string]any, body []byte) string {
		t.Helper()
		args["bytes"], args["sha256"] = float64(len(body)), sha256hex(body)
		up, _ := alice.call(t, "amp_publish_artifact", args)["upload"].(map[string]any)
		url, _ := up["url"].(string)
		if !strings.Contains(url, service.ArtifactUploadPath) {
			t.Fatalf("amp_publish_artifact with bytes+sha256 returned no upload url: %v", up)
		}
		return url
	}

	v1 := page("v1")
	url := mint(map[string]any{"task_id": float64(task), "title": "Screenshot mockup", "kind": "html"}, v1)
	if got := intField(t, alice.call(t, "amp_list_artifacts", map[string]any{"task_id": float64(task)}), "count"); got != 0 {
		t.Fatalf("minting published something: %d artifacts", got)
	}
	code, out := put(url, v1)
	if code != 200 {
		t.Fatalf("sessionless upload: %d %v, want 200", code, out)
	}
	art := out["artifact"].(map[string]any)
	id := art["id"].(float64)
	if out["version"].(float64) != 1 || art["task_id"].(float64) != float64(task) {
		t.Fatalf("v1 upload published %v", out)
	}
	if code, _ := put(url+"x", v1); code != 404 {
		t.Fatalf("a made-up token: %d, want 404", code)
	}

	v2 := page("v2")
	code, out = put(mint(map[string]any{"artifact_id": id, "note": "v2"}, v2), v2)
	if code != 200 || out["version"].(float64) != 2 || out["artifact"].(map[string]any)["id"].(float64) != id {
		t.Fatalf("v2 upload: %d %v, want artifact %v v2", code, out, id)
	}
	got := alice.call(t, "amp_get_artifact", map[string]any{"artifact_id": id, "include_content": false})
	if n := len(got["versions"].([]any)); n != 2 {
		t.Fatalf("artifact has %d versions, want 2", n)
	}
}

// Inline content keeps working for every existing caller; the upload route
// is an alternative, and mixing or omitting both is refused by name.
func TestArtifactUpload_InlineContentStillPublishes(t *testing.T) {
	w := newRBACWorld(t)
	alice := w.as(w.alice)
	task := mkIn(t, alice, w.s.storyID, "inline mockup")
	html := "<!doctype html><p>inline</p>"
	art, _ := alice.call(t, "amp_publish_artifact", map[string]any{"task_id": float64(task), "title": "Inline", "kind": "html",
		"content": html})["artifact"].(map[string]any)
	if art == nil || art["current_version"].(float64) != 1 {
		t.Fatalf("inline publish = %v, want v1", art)
	}
	both := alice.callExpectError(t, "amp_publish_artifact", map[string]any{"task_id": float64(task), "title": "X", "kind": "html",
		"content": html, "bytes": float64(len(html)), "sha256": sha256hex([]byte(html))})
	if !strings.Contains(both, "not both") {
		t.Fatalf("content plus bytes/sha256: %q, want a 'not both' refusal", both)
	}
	if neither := alice.callExpectError(t, "amp_publish_artifact", map[string]any{"task_id": float64(task), "title": "X", "kind": "html"}); !strings.Contains(neither, "upload URL") {
		t.Fatalf("no content at all: %q, want it to name both ways", neither)
	}
}
