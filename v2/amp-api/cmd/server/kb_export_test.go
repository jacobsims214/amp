package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/repository"
)

// A project export carries every live KB doc (no 250 cap) with its
// annotations, reason and original dates, and importing it into a fresh
// project recreates the same docs in Postgres and in the search index,
// without re-dating them (#1992).
func TestKB_ExportImportRoundTrip(t *testing.T) {
	s := setupMCP(t)
	if s.kb == nil {
		t.Skip("set TEST_TYPESENSE_URL to run against Typesense")
	}
	ctx := context.Background()
	src := kb.Project(s.projectID)

	const n = 260
	base := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC).Unix()
	for i := 0; i < n; i++ {
		d := domain.ExportKBDoc{
			Path: fmt.Sprintf("notes/%03d.md", i), Title: fmt.Sprintf("Note %d", i),
			Content: fmt.Sprintf("Body of note %d about zebrafish%d.", i, i), Tags: []string{"note", fmt.Sprintf("t%d", i%3)},
			Author: "writer@example.com", CreatedAt: base - 86400, UpdatedAt: base + int64(i)*60,
		}
		if i%10 == 0 {
			d.Annotations = []domain.KBAnnotation{
				{Author: "a@example.com", Text: fmt.Sprintf("note on %d", i), CreatedAt: base + 5},
				{Author: "b@example.com", Text: "resolved one", CreatedAt: base + 6, IsResolved: true, ResolvedBy: "c@example.com", ResolvedAt: base + 7},
			}
		}
		if err := s.kb.ImportDoc(ctx, src, d); err != nil {
			t.Fatalf("seed %s: %v", d.Path, err)
		}
	}
	// A doc deleted in the source must not be exported.
	if err := s.kb.DeleteDoc(ctx, src, "notes/005.md", "me"); err != nil {
		t.Fatal(err)
	}

	bundle := s.call(t, "amp_export_project", map[string]interface{}{"project_id": float64(s.projectID)})
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var parsed domain.ExportBundle
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.KBDocs) != n-1 {
		t.Fatalf("export has %d KB docs, want %d", len(parsed.KBDocs), n-1)
	}
	exported := map[string]domain.ExportKBDoc{}
	for _, d := range parsed.KBDocs {
		exported[d.Path] = d
	}
	if _, ok := exported["notes/005.md"]; ok {
		t.Error("export includes a deleted doc")
	}
	if d := exported["notes/250.md"]; d.UpdatedAt != base+250*60 || d.CreatedAt != base-86400 || len(d.Annotations) != 2 {
		t.Errorf("exported notes/250.md = %+v; want its original dates and 2 annotations", d)
	}

	code := fmt.Sprintf("imp-%d", time.Now().UnixNano())
	imported := s.call(t, "amp_import_project", map[string]interface{}{"bundle_json": string(raw), "code": code})
	if got := imported["kb_docs_imported"].(float64); int(got) != n-1 {
		t.Fatalf("kb_docs_imported = %v, want %d (failed: %v)", got, n-1, imported["kb_docs_failed"])
	}
	newID := int(imported["project"].(map[string]interface{})["id"].(float64))
	dst := kb.Project(newID)
	t.Cleanup(func() { s.kb.Drop(context.Background(), dst) }) //nolint:errcheck

	if c, err := s.kb.CountDocs(ctx, dst); err != nil || c != n-1 {
		t.Fatalf("imported project has %d docs, %v; want %d", c, err, n-1)
	}
	for path, want := range exported {
		row, err := s.repo.GetKBDocument(ctx, kbStoreScope(dst), path)
		if err != nil {
			t.Fatalf("postgres row %s: %v", path, err)
		}
		if row.Title != want.Title || row.Content != want.Content || row.Author != want.Author ||
			!slices.Equal(row.Tags, want.Tags) || row.UpdatedAt.Unix() != want.UpdatedAt || row.CreatedAt.Unix() != want.CreatedAt ||
			len(row.Annotations) != len(want.Annotations) {
			t.Fatalf("imported %s = %+v; want %+v", path, row, want)
		}
		for i := range want.Annotations {
			if row.Annotations[i] != want.Annotations[i] {
				t.Fatalf("imported %s annotation %d = %+v; want %+v", path, i, row.Annotations[i], want.Annotations[i])
			}
		}
	}

	// The index has them too, with the original updated_at.
	for _, i := range []int{0, 129, 259} {
		path := fmt.Sprintf("notes/%03d.md", i)
		res := s.call(t, "amp_kb_search", map[string]interface{}{"project_id": float64(newID), "query": fmt.Sprintf("zebrafish%d", i)})
		found := false
		for _, r := range res["results"].([]interface{}) {
			hit := r.(map[string]interface{})
			if hit["path"] == path {
				found = true
				if int64(hit["updated_at"].(float64)) != base+int64(i)*60 {
					t.Errorf("search hit %s updated_at = %v, want %d", path, hit["updated_at"], base+int64(i)*60)
				}
			}
		}
		if !found {
			t.Errorf("search in the imported project misses %s", path)
		}
	}
}

// An export made before #1992 has no reason or dates on its KB docs. It
// still imports: the docs get their annotations and are dated now.
func TestKB_ImportOldFormatExport(t *testing.T) {
	s := setupMCP(t)
	if s.kb == nil {
		t.Skip("set TEST_TYPESENSE_URL to run against Typesense")
	}
	ctx := context.Background()
	// The bundle of a project with no work items, as exports wrote it before.
	oldBundle := `{
		"version": "1",
		"exported_at": "2026-09-01T10:00:00Z",
		"project": {"id": 999999, "name": "Old export", "code": "old"},
		"epics": [], "stories": [], "tasks": [],
		"kb_docs": [
			{"path": "architecture/old.md", "title": "Old", "content": "Old format body about quokkas.",
			 "tags": ["old"], "author": "old@example.com",
			 "annotations": [{"author": "x@example.com", "text": "kept", "created_at": 1700000000, "is_resolved": false}]},
			{"path": "plain.md", "title": "Plain", "content": "No annotations here.", "tags": [], "author": "old@example.com"}
		]
	}`
	before := time.Now().Add(-time.Second).Unix()
	imported := s.call(t, "amp_import_project", map[string]interface{}{
		"bundle_json": oldBundle, "code": fmt.Sprintf("old-%d", time.Now().UnixNano()), "name": "Old export import"})
	if got := imported["kb_docs_imported"].(float64); got != 2 {
		t.Fatalf("kb_docs_imported = %v, want 2 (failed: %v)", got, imported["kb_docs_failed"])
	}
	newID := int(imported["project"].(map[string]interface{})["id"].(float64))
	dst := kb.Project(newID)
	t.Cleanup(func() { s.kb.Drop(context.Background(), dst) }) //nolint:errcheck

	doc, err := s.kb.GetDoc(ctx, dst, "architecture/old.md")
	if err != nil || doc.Content != "Old format body about quokkas." || doc.Author != "old@example.com" ||
		len(doc.Annotations) != 1 || doc.Annotations[0].Text != "kept" {
		t.Fatalf("imported old doc = %+v, %v", doc, err)
	}
	if doc.UpdatedAt < before {
		t.Errorf("old-format doc updated_at = %d, want now (>= %d)", doc.UpdatedAt, before)
	}
	row, err := s.repo.GetKBDocument(ctx, kbStoreScope(dst), "architecture/old.md")
	if err != nil || !row.CreatedAt.Equal(row.UpdatedAt) {
		t.Errorf("old-format row created_at %v, updated_at %v, %v; want equal", row.CreatedAt, row.UpdatedAt, err)
	}
	if _, err := s.kb.GetDoc(ctx, dst, "plain.md"); err != nil {
		t.Errorf("plain.md: %v", err)
	}
	res := s.call(t, "amp_kb_search", map[string]interface{}{"project_id": float64(newID), "query": "quokkas"})
	if hits := res["results"].([]interface{}); len(hits) == 0 {
		t.Error("old-format doc is not searchable after import")
	}
}

func kbStoreScope(sc kb.Scope) repository.KBScope { return repository.KBScope{Kind: sc.Kind, ID: sc.ID} }
