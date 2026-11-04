package main

import (
	"fmt"
	"testing"
	"time"
)

// amp_kb_list_deleted and amp_kb_restore (#2018): a doc deleted with
// amp_kb_delete shows in the trash listing, and restoring it brings it back
// readable and searchable with its annotations.
func TestKB_DeleteListDeletedRestore(t *testing.T) {
	s := setupMCP(t)
	if s.kb == nil {
		t.Skip("set TEST_TYPESENSE_URL to run against Typesense")
	}
	pid := float64(s.projectID)
	word := fmt.Sprintf("axolotl%d", time.Now().UnixNano())
	path := "notes/restore-me.md"
	s.call(t, "amp_kb_write", map[string]interface{}{
		"project_id": pid, "path": path, "title": "Restore me", "content": "This doc mentions " + word + ".",
		"tags": []interface{}{"a", "b", "c"},
	})
	s.call(t, "amp_kb_write", map[string]interface{}{
		"project_id": pid, "path": "notes/keep.md", "title": "Keep", "content": "stays live", "tags": []interface{}{"a", "b", "c"},
	})
	s.call(t, "amp_kb_annotate", map[string]interface{}{"project_id": pid, "path": path, "text": "a note that must survive"})

	searchHits := func() bool {
		res := s.call(t, "amp_kb_search", map[string]interface{}{"project_id": pid, "query": word})
		for _, r := range res["results"].([]interface{}) {
			if r.(map[string]interface{})["path"] == path {
				return true
			}
		}
		return false
	}
	if !searchHits() {
		t.Fatalf("search misses %s before the delete", path)
	}

	// Nothing in the trash yet.
	if res := s.call(t, "amp_kb_list_deleted", map[string]interface{}{"project_id": pid}); res["count"] != float64(0) {
		t.Fatalf("list_deleted before any delete = %v", res)
	}

	s.call(t, "amp_kb_delete", map[string]interface{}{"project_id": pid, "path": path})
	wantErr(t, s.callExpectError(t, "amp_kb_get", map[string]interface{}{"project_id": pid, "path": path}), "not found")
	if searchHits() {
		t.Fatalf("search still finds %s after the delete", path)
	}

	res := s.call(t, "amp_kb_list_deleted", map[string]interface{}{"project_id": pid})
	docs := res["docs"].([]interface{})
	if res["count"] != float64(1) || len(docs) != 1 {
		t.Fatalf("list_deleted = %v; want just %s", res, path)
	}
	d := docs[0].(map[string]interface{})
	if d["path"] != path || d["title"] != "Restore me" || d["deleted_by"] == "" || d["deleted_at"] == nil {
		t.Fatalf("deleted doc = %v", d)
	}
	if at := int64(d["deleted_at"].(float64)); time.Since(time.Unix(at, 0)) > time.Minute {
		t.Errorf("deleted_at = %d, not just now", at)
	}

	restored := s.call(t, "amp_kb_restore", map[string]interface{}{"project_id": pid, "path": path})
	if restored["restored"] != true {
		t.Fatalf("restore = %v", restored)
	}
	got := s.call(t, "amp_kb_get", map[string]interface{}{"project_id": pid, "path": path})
	if got["title"] != "Restore me" || got["chunk_text"] != "This doc mentions "+word+"." {
		t.Fatalf("restored doc = %v", got)
	}
	if anns, _ := got["annotations"].([]interface{}); len(anns) != 1 {
		t.Errorf("restored annotations = %v; want the one note", got["annotations"])
	}
	if !searchHits() {
		t.Fatalf("search misses %s after the restore", path)
	}
	if res := s.call(t, "amp_kb_list_deleted", map[string]interface{}{"project_id": pid}); res["count"] != float64(0) {
		t.Errorf("list_deleted after the restore = %v", res)
	}

	// A path with nothing in the trash: a clear error, not a silent success.
	wantErr(t, s.callExpectError(t, "amp_kb_restore", map[string]interface{}{"project_id": pid, "path": path}), "no deleted doc")
	wantErr(t, s.callExpectError(t, "amp_kb_restore", map[string]interface{}{"project_id": pid, "path": "never/was.md"}), "no deleted doc")
}

// Both tools are listed and use amp_kb_delete's access rules: write access
// to the KB they name, and never the global KB.
func TestKB_RestoreToolsAccess(t *testing.T) {
	w := newRBACWorld(t)
	pid := float64(w.s.projectID)
	inProject := map[string]interface{}{"project_id": pid}
	org := map[string]interface{}{"scope": "org", "org_id": float64(w.org.ID)}
	global := map[string]interface{}{"project_id": pid, "scope": "global"}

	for _, as := range []*mcpStack{w.as(w.alice), w.as(w.root)} {
		listed := names(as.listTools(t))
		for _, n := range []string{"amp_kb_list_deleted", "amp_kb_restore"} {
			if !listed[n] {
				t.Errorf("%s not listed", n)
			}
		}
	}

	for _, tool := range []string{"amp_kb_list_deleted", "amp_kb_restore"} {
		args := func(base map[string]interface{}) map[string]interface{} {
			if tool == "amp_kb_restore" {
				return kbArgs(base, "path", "p.md")
			}
			return base
		}
		wantErr(t, w.as(w.alice).callExpectError(t, tool, args(global)), "AMP guide is read-only")
		wantErr(t, w.as(w.root).callExpectError(t, tool, args(global)), "AMP guide is read-only")
		wantErr(t, w.as(w.alice).callExpectError(t, tool, args(org)), "only org owners and admins")
		wantErr(t, w.as(w.outsider).callExpectError(t, tool, args(inProject)), "not found")
		if tool == "amp_kb_list_deleted" {
			kbAllowed(t, w.as(w.alice), tool, inProject)
			kbAllowed(t, w.as(w.orgadmin), tool, org)
		}
	}
}
