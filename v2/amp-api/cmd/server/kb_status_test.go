package main

import (
	"testing"
)

// amp_kb_status reports stored_docs, indexed_docs and the backfill state
// next to the existing fields (#1995); the global KB has no stored count.
func TestKB_StatusReportsStoredAndIndexed(t *testing.T) {
	s := setupMCP(t)
	if s.kb == nil {
		t.Skip("set TEST_TYPESENSE_URL to run against Typesense")
	}
	pid := float64(s.projectID)
	for _, p := range []string{"a.md", "b.md"} {
		s.call(t, "amp_kb_write", map[string]interface{}{
			"project_id": pid, "path": p, "title": p, "content": "body of " + p, "tags": []interface{}{"x", "y", "z"},
		})
	}
	st := s.call(t, "amp_kb_status", map[string]interface{}{"project_id": pid})
	for _, k := range []string{"total_docs", "stale_docs", "docs_with_annotations", "total_annotations", "unresolved_annotations", "docs_by_tag"} {
		if _, ok := st[k]; !ok {
			t.Errorf("existing field %q missing: %v", k, st)
		}
	}
	if st["total_docs"] != float64(2) || st["stored_docs"] != float64(2) || st["indexed_docs"] != float64(2) {
		t.Errorf("status = %v; want total, stored and indexed 2", st)
	}
	b, _ := st["backfill"].(map[string]interface{})
	if b["status"] != "complete" { // a new project's KB has no collection to copy, so it is complete on first use
		t.Errorf("backfill = %v", st["backfill"])
	}

	g := s.call(t, "amp_kb_status", map[string]interface{}{"project_id": pid, "scope": "global"})
	if _, ok := g["stored_docs"]; ok || g["note"] == nil || g["indexed_docs"] == nil || g["total_docs"] == nil {
		t.Errorf("global status = %v", g)
	}
}
