package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/simstech/amp-api/internal/kb"
)

// amp_kb_list returns one page, the newest 250 by default, with total and
// next_offset; kb, docs and count keep their meaning (#2028).
func TestKB_ListPages(t *testing.T) {
	s := setupMCP(t)
	if s.kb == nil {
		t.Skip("set TEST_TYPESENSE_URL to run against Typesense")
	}
	ctx := context.Background()
	sc := kb.Project(s.projectID)
	const n = 300
	for i := 0; i < n; i++ {
		tags := []string{"all"}
		if i%3 == 0 {
			tags = append(tags, "third")
		}
		p := fmt.Sprintf("docs/%03d.md", i)
		if _, err := s.kb.WriteDoc(ctx, sc, p, "Doc "+p, "body of "+p, "me", tags, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	pid := float64(s.projectID)

	paths := func(res map[string]interface{}) []string {
		docs, _ := res["docs"].([]interface{})
		out := make([]string, 0, len(docs))
		for _, d := range docs {
			out = append(out, d.(map[string]interface{})["path"].(string))
		}
		return out
	}

	first := s.call(t, "amp_kb_list", map[string]interface{}{"project_id": pid})
	for _, k := range []string{"kb", "docs", "count"} {
		if _, ok := first[k]; !ok {
			t.Errorf("key %q missing: %v", k, first["count"])
		}
	}
	got := paths(first)
	if len(got) != 250 || first["count"] != float64(250) || first["total"] != float64(n) || first["next_offset"] != float64(250) {
		t.Fatalf("no args: %d docs, count %v, total %v, next_offset %v; want 250, 250, 300, 250",
			len(got), first["count"], first["total"], first["next_offset"])
	}
	if got[0] != "docs/299.md" || got[249] != "docs/050.md" {
		t.Errorf("first page = %s .. %s; want the newest 250 (docs/299.md .. docs/050.md)", got[0], got[249])
	}

	rest := s.call(t, "amp_kb_list", map[string]interface{}{"project_id": pid, "offset": float64(250)})
	got2 := paths(rest)
	if len(got2) != 50 || rest["count"] != float64(50) || rest["total"] != float64(n) {
		t.Errorf("offset 250: %d docs, count %v, total %v; want 50, 50, 300", len(got2), rest["count"], rest["total"])
	}
	if _, ok := rest["next_offset"]; ok {
		t.Errorf("last page has next_offset %v", rest["next_offset"])
	}
	if len(got2) > 0 && (got2[0] != "docs/049.md" || got2[len(got2)-1] != "docs/000.md") {
		t.Errorf("second page = %s .. %s", got2[0], got2[len(got2)-1])
	}

	tagged := s.call(t, "amp_kb_list", map[string]interface{}{"project_id": pid, "tag": "third", "limit": float64(40)})
	if len(paths(tagged)) != 40 || tagged["total"] != float64(n/3) || tagged["next_offset"] != float64(40) {
		t.Errorf("tag third, limit 40: %d docs, total %v, next_offset %v; want 40, 100, 40",
			len(paths(tagged)), tagged["total"], tagged["next_offset"])
	}

	// limit is capped at 1000, so a big limit lists the whole KB here.
	all := s.call(t, "amp_kb_list", map[string]interface{}{"project_id": pid, "limit": float64(5000)})
	if len(paths(all)) != n || all["total"] != float64(n) {
		t.Errorf("limit 5000: %d docs, total %v; want all %d", len(paths(all)), all["total"], n)
	}
	if _, ok := all["next_offset"]; ok {
		t.Errorf("whole listing has next_offset %v", all["next_offset"])
	}
}
