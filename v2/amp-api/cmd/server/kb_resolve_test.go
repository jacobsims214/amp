package main

import (
	"fmt"
	"testing"
)

// Annotations are resolved once worked into the doc: by whoever can edit
// the KB, recorded, kept as history, reversible, and hidden from search
// previews so agents don't re-apply old corrections.
func TestKB_ResolveAnnotations(t *testing.T) {
	w := newRBACWorld(t)
	if w.s.kb == nil {
		t.Skip("set TEST_TYPESENSE_URL to run against Typesense")
	}
	org := map[string]interface{}{"scope": "org", "org_id": float64(w.org.ID)}
	args := func(kv ...interface{}) map[string]interface{} { return kbArgs(org, kv...) }
	const path = "brand/colours.md"

	w.as(w.orgadmin).call(t, "amp_kb_write", args("path", path, "title", "Brand colours", "content", "Indigo and slate palette.",
		"tags", []interface{}{"brand"}, "reason", "Brand colours apply to every product in the org."))
	w.as(w.alice).call(t, "amp_kb_annotate", args("path", path, "text", "Slate changed in v3."))
	w.as(w.carol).call(t, "amp_kb_annotate", args("path", path, "text", "Indigo is now #4F46E5."))

	preview := func() string {
		t.Helper()
		res := w.as(w.alice).call(t, "amp_kb_search", args("query", "palette"))
		hits := res["results"].([]interface{})
		if len(hits) == 0 {
			t.Fatal("no search hits")
		}
		latest, _ := hits[0].(map[string]interface{})["latest_annotation"].(string)
		return latest
	}
	if p := preview(); p != "Indigo is now #4F46E5." {
		t.Fatalf("preview before resolving = %q", p)
	}

	// Members annotate the org KB, but only owners and admins resolve.
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_kb_resolve_annotation", args("path", path, "index", float64(1))), "only org owners and admins")
	res := w.as(w.orgadmin).call(t, "amp_kb_resolve_annotation", args("path", path, "index", float64(1)))
	anns := res["annotations"].([]interface{})
	second := anns[1].(map[string]interface{})
	if second["is_resolved"] != true || second["resolved_by"] != w.orgadmin.Email || second["resolved_at"] == nil {
		t.Fatalf("resolved annotation = %v", second)
	}
	if anns[0].(map[string]interface{})["is_resolved"] != false {
		t.Fatal("resolving one annotation touched another")
	}
	// The preview falls back to the newest unresolved note.
	if p := preview(); p != "Slate changed in v3." {
		t.Fatalf("preview after resolving the newest = %q", p)
	}
	w.as(w.orgadmin).call(t, "amp_kb_resolve_annotation", args("path", path, "index", float64(0)))
	if p := preview(); p != "" {
		t.Fatalf("preview with everything resolved = %q", p)
	}

	// Resolution survives a rewrite, and is reversible.
	w.as(w.orgadmin).call(t, "amp_kb_write", args("path", path, "title", "Brand colours", "content", "Indigo #4F46E5 and the v3 slate palette.",
		"tags", []interface{}{"brand"}, "reason", "Brand colours apply to every product in the org."))
	got := w.as(w.alice).call(t, "amp_kb_get", args("path", path))
	for i, a := range got["annotations"].([]interface{}) {
		if a.(map[string]interface{})["is_resolved"] != true {
			t.Fatalf("annotation %d lost its resolution on rewrite: %v", i, a)
		}
	}
	res = w.as(w.orgadmin).call(t, "amp_kb_resolve_annotation", args("path", path, "index", float64(0), "resolved", false))
	if a := res["annotations"].([]interface{})[0].(map[string]interface{}); a["is_resolved"] != false || a["resolved_by"] != nil {
		t.Fatalf("reopened annotation = %v", a)
	}

	wantErr(t, w.as(w.orgadmin).callExpectError(t, "amp_kb_resolve_annotation", args("path", path, "index", float64(5))), "out of range")
	wantErr(t, w.as(w.orgadmin).callExpectError(t, "amp_kb_resolve_annotation", args("path", "nope.md", "index", float64(0))), "not found")

	// REST: the same rules for the UI.
	ra := newRestAs(t, w)
	route := fmt.Sprintf("/api/kb/resolve?org_id=%d", w.org.ID)
	ra.want(t, 403, "alice", "POST", route, map[string]any{"path": path, "index": 0})
	ra.want(t, 400, "orgadmin", "POST", route, map[string]any{"path": path})
	ra.want(t, 404, "orgadmin", "POST", route, map[string]any{"path": "nope.md", "index": 0})
	doc := ra.want(t, 200, "orgadmin", "POST", route, map[string]any{"path": path, "index": 0})
	if a := doc["annotations"].([]any)[0].(map[string]any); a["is_resolved"] != true || a["resolved_by"] != w.orgadmin.Email {
		t.Fatalf("REST resolve = %v", a)
	}
}
