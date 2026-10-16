package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/queue"
)

// Annotations are notes people and agents leave on a KB doc. Nothing that
// rewrites the doc may lose them: an update, a reindex (including one that
// rebuilds the collection), a queued write, or an export/import.
func TestKB_AnnotationsSurvive(t *testing.T) {
	s := setupMCP(t)
	if s.kb == nil {
		t.Skip("set TEST_TYPESENSE_URL to run against Typesense")
	}
	ctx := context.Background()
	sc := kb.Project(s.projectID)
	pid := float64(s.projectID)
	const path = "architecture/auth.md"

	write := func(content string) {
		t.Helper()
		s.call(t, "amp_kb_write", map[string]interface{}{"project_id": pid, "path": path, "title": "Auth",
			"content": content, "tags": []interface{}{"auth"}})
	}
	want := func(stage string, n int) {
		t.Helper()
		doc, err := s.kb.GetDoc(ctx, sc, path)
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if len(doc.Annotations) != n {
			t.Fatalf("%s: %d annotations, want %d: %+v", stage, len(doc.Annotations), n, doc.Annotations)
		}
		res := s.call(t, "amp_kb_search", map[string]interface{}{"project_id": pid, "query": "tokens"})
		for _, r := range res["results"].([]interface{}) {
			if c := r.(map[string]interface{})["annotation_count"].(float64); int(c) != n {
				t.Fatalf("%s: search hit shows %v annotations, want %d", stage, c, n)
			}
		}
	}

	write("Auth uses signed tokens.")
	for _, text := range []string{"Tokens expire after 1h now.", "Refresh tokens rotate."} {
		s.call(t, "amp_kb_annotate", map[string]interface{}{"project_id": pid, "path": path, "text": text})
	}
	want("after annotating", 2)

	write("Auth uses signed tokens, verified at the gateway.")
	want("after a rewrite", 2)

	if err := s.kb.Reindex(ctx, sc); err != nil {
		t.Fatal(err)
	}
	want("after a reindex", 2)

	// A reindex that rebuilds the collection, and an import, write into a KB
	// that no longer holds the doc: the annotations are carried explicitly.
	doc, _ := s.kb.GetDoc(ctx, sc, path)
	if err := s.kb.Drop(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.kb.WriteDoc(ctx, sc, doc.Path, doc.Title, doc.Content, doc.Author, doc.Tags, "", doc.Annotations); err != nil {
		t.Fatal(err)
	}
	want("after rebuilding the collection", 2)

	// The queued-write worker carries them too.
	payload, _ := json.Marshal(queue.WriteDocPayload{ProjectID: s.projectID, Path: "queued.md", Title: "Queued",
		Content: "Queued tokens doc.", Tags: []string{"q"}, Annotations: annJSON(t, doc.Annotations)})
	if err := s.kb.HandleWriteDocTask(ctx, asynq.NewTask(queue.TypeKBWriteDoc, payload)); err != nil {
		t.Fatal(err)
	}
	if q, err := s.kb.GetDoc(ctx, sc, "queued.md"); err != nil || len(q.Annotations) != 2 {
		t.Fatalf("queued write: %v %+v", err, q)
	}

	// Export and import: the bundle carries them, and the new project has them.
	bundle := s.call(t, "amp_export_project", map[string]interface{}{"project_id": pid})
	raw, _ := json.Marshal(bundle)
	code := fmt.Sprintf("imp-%d", time.Now().UnixNano())
	imported := s.call(t, "amp_import_project", map[string]interface{}{"bundle_json": string(raw), "code": code})
	newID := int(imported["project"].(map[string]interface{})["id"].(float64))
	got, err := s.kb.GetDoc(ctx, kb.Project(newID), path)
	if err != nil || len(got.Annotations) != 2 || got.Annotations[0].Text != "Tokens expire after 1h now." {
		t.Fatalf("imported doc: %v %+v", err, got)
	}
}

func annJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
