package main

import "testing"

// Agent reads (#159): amp_kb_get and amp_kb_search count a read of the doc,
// shown by the next amp_kb_get and in amp_kb_list.
func TestKB_AgentReadsRecorded(t *testing.T) {
	s := setupMCP(t)
	if s.kb == nil {
		t.Skip("set TEST_TYPESENSE_URL to run against Typesense")
	}
	pid := float64(s.projectID)
	path := "notes/read-me.md"
	s.call(t, "amp_kb_write", map[string]interface{}{
		"project_id": pid, "path": path, "title": "Read me", "content": "Quokka habitat notes.",
		"tags": []interface{}{"a", "b", "c"},
	})

	first := s.call(t, "amp_kb_get", map[string]interface{}{"project_id": pid, "path": path})
	if first["created_at"] == nil || first["read_count"] != float64(0) {
		t.Fatalf("first get (shows reads before it) = created %v, count %v", first["created_at"], first["read_count"])
	}
	second := s.call(t, "amp_kb_get", map[string]interface{}{"project_id": pid, "path": path})
	if second["read_count"] != float64(1) || second["last_read_by"] == nil || second["last_read_at"] == nil {
		t.Fatalf("second get = count %v by %v at %v; want the first read", second["read_count"], second["last_read_by"], second["last_read_at"])
	}

	list := s.call(t, "amp_kb_list", map[string]interface{}{"project_id": pid})
	for _, d := range list["docs"].([]interface{}) {
		if m := d.(map[string]interface{}); m["path"] == path && m["read_count"] != float64(1) {
			t.Fatalf("listed read_count = %v; want 1 (repeat get throttled)", m["read_count"])
		}
	}
}
