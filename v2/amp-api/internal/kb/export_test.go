package kb

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// Before its backfill completes, an export of a stored KB includes the docs
// only the index has (past the first 250), Postgres wins for a path both
// have, and a doc deleted in Postgres stays out even though the index still
// lists it. After the backfill, Postgres alone answers.
func TestExportDocsBeforeBackfill(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	rs := storeScope(sc)

	const indexOnly = 255
	for i := 0; i < indexOnly; i++ {
		if _, err := s.indexChunks(ctx, sc, indexedDoc{
			path: fmt.Sprintf("old/%03d.md", i), title: "Old", content: fmt.Sprintf("old body %d", i), author: "me",
			tags: []string{"old"}, updatedAt: int64(1_700_000_000 + i),
			annotations: []Annotation{{Author: "x", Text: "on old", CreatedAt: 1_700_000_000}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "both.md", title: "Stale", content: "stale", updatedAt: 1_700_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.UpsertKBDocument(ctx, rs, repository.KBDocWrite{Path: "both.md", Title: "Fresh", Content: "fresh", Reason: "why", By: "me"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.UpsertKBDocument(ctx, rs, repository.KBDocWrite{Path: "gone.md", Title: "Gone", Content: "gone", By: "me"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteKBDocument(ctx, rs, "gone.md", "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "gone.md", title: "Gone", content: "gone", updatedAt: 1_800_000_000}); err != nil {
		t.Fatal(err)
	}

	docs, err := s.ExportDocs(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != indexOnly+1 {
		t.Fatalf("ExportDocs = %d docs, want %d", len(docs), indexOnly+1)
	}
	byPath := map[string]domain.ExportKBDoc{}
	for _, d := range docs {
		byPath[d.Path] = d
	}
	if _, ok := byPath["gone.md"]; ok {
		t.Error("export includes a doc deleted in Postgres")
	}
	if d := byPath["both.md"]; d.Title != "Fresh" || d.Content != "fresh" || d.Reason != "why" || d.CreatedAt == 0 {
		t.Errorf("both.md = %+v; want the Postgres copy with its created_at", d)
	}
	if d := byPath["old/254.md"]; d.Content != "old body 254" || d.UpdatedAt != 1_700_000_254 || len(d.Annotations) != 1 {
		t.Errorf("old/254.md = %+v; want the index copy with content, date and annotation", d)
	}
	for i := 1; i < len(docs); i++ {
		if docs[i].UpdatedAt > docs[i-1].UpdatedAt {
			t.Fatalf("export not newest first at %d", i)
		}
	}

	if err := repo.CompleteKBBackfill(ctx, rs, 1); err != nil {
		t.Fatal(err)
	}
	if docs, err := s.ExportDocs(ctx, sc); err != nil || len(docs) != 1 || docs[0].Path != "both.md" {
		t.Errorf("ExportDocs after backfill = %+v, %v; want both.md only", docs, err)
	}
}

// ImportDoc never overwrites: a path with a live or deleted row is left as
// it was and ErrDocExists returned. A doc only the index has also counts.
func TestImportDocLeavesExistingDocs(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	rs := storeScope(sc)

	// A pre-existing KB: its collection (with an index-only doc) is there
	// before the first Postgres write. (A KB with no collection at its first
	// write is marked backfilled, #1994.)
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "indexed.md", title: "Indexed", content: "index body", updatedAt: 1_700_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteDoc(ctx, sc, "live.md", "Live", "live body", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteDoc(ctx, sc, "deleted.md", "Deleted", "deleted body", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDoc(ctx, sc, "deleted.md", "me"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"live.md", "deleted.md", "indexed.md"} {
		err := s.ImportDoc(ctx, sc, domain.ExportKBDoc{Path: path, Title: "Imported", Content: "imported", UpdatedAt: 1_600_000_000})
		if !errors.Is(err, ErrDocExists) {
			t.Errorf("ImportDoc(%s) = %v, want ErrDocExists", path, err)
		}
	}
	if d := mustRow(t, repo, sc, "live.md"); d.Content != "live body" {
		t.Errorf("live.md was overwritten: %+v", d)
	}
	if _, err := repo.GetKBDocument(ctx, rs, "deleted.md"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("deleted.md was revived: %v", err)
	}
	if del, err := repo.ListDeletedKBPaths(ctx, rs); err != nil || len(del) != 1 || del[0] != "deleted.md" {
		t.Errorf("deleted paths = %v, %v; want [deleted.md]", del, err)
	}
	if d, err := s.RestoreDoc(ctx, sc, "deleted.md", "me"); err != nil || d.Content != "deleted body" {
		t.Errorf("restored deleted.md = %+v, %v; want its own content", d, err)
	}
	if d := mustRow(t, repo, sc, "indexed.md"); d.Content != "index body" || d.UpdatedAt.Unix() != 1_700_000_000 {
		t.Errorf("indexed.md = %+v; want the adopted index copy", d)
	}
	if err := s.ImportDoc(ctx, sc, domain.ExportKBDoc{Title: "No path"}); err == nil {
		t.Error("ImportDoc with no path succeeded")
	}
	if n, err := repo.CountKBDocuments(ctx, rs); err != nil || n != 3 {
		t.Errorf("live docs = %d, %v; want 3", n, err)
	}
}
