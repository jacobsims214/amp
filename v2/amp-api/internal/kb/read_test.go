package kb

// Tests for the Postgres read path (#1989). Same setup as store_test.go:
// TEST_DATABASE_URL and TEST_TYPESENSE_URL.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/simstech/amp-api/internal/repository"
)

// Reads of a backfilled KB come from Postgres alone: with Typesense
// unreachable, a doc reads back, lists and counts right after it is
// written, annotations included.
func TestStoredReadsComeFromPostgres(t *testing.T) {
	repo := storeTestRepo(t)
	s := New("http://127.0.0.1:1", "x", "", "")
	s.SetStore(repo)
	sc := Team(1_000_000 + rand.IntN(1_000_000_000))
	ctx := context.Background()
	if err := repo.CompleteKBBackfill(ctx, storeScope(sc), 0); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.md", "b.md"} {
		if _, err := s.WriteDoc(ctx, sc, p, "Title "+p, "body of "+p, "me", []string{"x", p}, "why", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AnnotateDoc(ctx, sc, "a.md", "note", "me"); err != nil {
		t.Fatal(err)
	}

	doc, err := s.GetDoc(ctx, sc, "a.md")
	if err != nil {
		t.Fatal(err)
	}
	row := mustRow(t, repo, sc, "a.md")
	if doc.Content != "body of a.md" || doc.Title != "Title a.md" || doc.Reason != "why" || doc.Scope != ScopeTeam ||
		doc.ScopeID != sc.ID || doc.ID != docID(sc, "a.md", 0) || doc.UpdatedAt != row.UpdatedAt.Unix() ||
		len(doc.Annotations) != 1 || doc.Annotations[0].Text != "note" || doc.ChunkText == "" {
		t.Errorf("GetDoc = %+v", doc)
	}
	if _, err := s.GetDoc(ctx, sc, "missing.md"); err == nil {
		t.Error("GetDoc of a missing doc succeeded")
	}

	docs, err := s.ListDocs(ctx, sc, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].Path != "b.md" || docs[1].Path != "a.md" {
		t.Errorf("ListDocs = %+v, want b.md then a.md (newest first)", docs)
	}
	if docs[1].Reason != "why" || docs[1].ScopeID != sc.ID || docs[1].ID != docID(sc, "a.md", 0) {
		t.Errorf("summary = %+v", docs[1])
	}
	if tagged, err := s.ListDocs(ctx, sc, "a.md"); err != nil || len(tagged) != 1 || tagged[0].Path != "a.md" {
		t.Errorf("ListDocs(tag) = %+v, %v", tagged, err)
	}
	if n, err := s.CountDocs(ctx, sc); err != nil || n != 2 {
		t.Errorf("CountDocs = %d, %v; want 2", n, err)
	}
	st, err := s.KBStatus(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalDocs != 2 || st.DocsWithAnnotations != 1 || st.UnresolvedAnnotations != 1 || st.DocsByTag["x"] != 2 {
		t.Errorf("KBStatus = %+v", st)
	}

	// A deleted doc is gone from every read.
	if err := s.DeleteDoc(ctx, sc, "b.md", "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDoc(ctx, sc, "b.md"); err == nil {
		t.Error("GetDoc of a deleted doc succeeded")
	}
	if n, _ := s.CountDocs(ctx, sc); n != 1 {
		t.Errorf("CountDocs after delete = %d, want 1", n)
	}
}

// Before a KB's backfill completes, reads merge in the docs only the index
// has (listed past Typesense's 250-per-page limit), skip index entries of
// docs deleted in Postgres, and prefer the Postgres copy of a path both
// have. Once the backfill completes, they read Postgres only.
func TestStoredReadsBeforeBackfill(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	rs := storeScope(sc)

	// 260 docs written before the Postgres store existed: index only.
	const indexOnly = 260
	for i := 0; i < indexOnly; i++ {
		tags := []string{"old"}
		if i%2 == 0 {
			tags = append(tags, "even")
		}
		if _, err := s.indexChunks(ctx, sc, indexedDoc{
			path: fmt.Sprintf("old/%03d.md", i), title: "Old", content: "old body", author: "me",
			tags: tags, updatedAt: int64(1_700_000_000 + i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A path both have: the Postgres copy is newer and wins.
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "both.md", title: "Stale", content: "stale", updatedAt: 1_700_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.UpsertKBDocument(ctx, rs, repository.KBDocWrite{Path: "both.md", Title: "Fresh", Content: "fresh", By: "me"}); err != nil {
		t.Fatal(err)
	}
	// Deleted in Postgres, but its index entry is still there (its removal
	// failed): it must not come back.
	if _, _, err := repo.UpsertKBDocument(ctx, rs, repository.KBDocWrite{Path: "gone.md", Title: "Gone", Content: "gone", By: "me"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteKBDocument(ctx, rs, "gone.md", "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "gone.md", title: "Gone", content: "gone", updatedAt: 1_800_000_000}); err != nil {
		t.Fatal(err)
	}

	docs, err := s.ListDocs(ctx, sc, "")
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]DocSummary{}
	for _, d := range docs {
		if _, dup := paths[d.Path]; dup {
			t.Errorf("ListDocs lists %s twice", d.Path)
		}
		paths[d.Path] = d
	}
	if len(docs) != indexOnly+1 {
		t.Errorf("ListDocs = %d docs, want %d", len(docs), indexOnly+1)
	}
	if _, ok := paths["gone.md"]; ok {
		t.Error("ListDocs lists a doc deleted in Postgres")
	}
	if paths["both.md"].Title != "Fresh" || docs[0].Path != "both.md" {
		t.Errorf("both.md = %+v, first = %s; want the Postgres copy, newest first", paths["both.md"], docs[0].Path)
	}
	if _, ok := paths["old/259.md"]; !ok {
		t.Error("ListDocs misses index-only docs past the first 250")
	}
	for i := 1; i < len(docs); i++ {
		if docs[i].UpdatedAt > docs[i-1].UpdatedAt {
			t.Fatalf("ListDocs not newest first at %d: %d > %d", i, docs[i].UpdatedAt, docs[i-1].UpdatedAt)
		}
	}
	if even, err := s.ListDocs(ctx, sc, "even"); err != nil || len(even) != indexOnly/2 {
		t.Errorf("ListDocs(even) = %d docs, %v; want %d", len(even), err, indexOnly/2)
	}
	if n, err := s.CountDocs(ctx, sc); err != nil || n != indexOnly+1 {
		t.Errorf("CountDocs = %d, %v; want %d", n, err, indexOnly+1)
	}
	if st, err := s.KBStatus(ctx, sc); err != nil || st.TotalDocs != indexOnly+1 || st.DocsByTag["old"] != indexOnly {
		t.Errorf("KBStatus = %+v, %v", st, err)
	}

	if d, err := s.GetDoc(ctx, sc, "old/007.md"); err != nil || d.Content != "old body" || d.Scope != ScopeTeam {
		t.Errorf("GetDoc of an index-only doc = %+v, %v", d, err)
	}
	if d, err := s.GetDoc(ctx, sc, "both.md"); err != nil || d.Content != "fresh" {
		t.Errorf("GetDoc(both.md) = %+v, %v; want the Postgres copy", d, err)
	}
	if _, err := s.GetDoc(ctx, sc, "gone.md"); err == nil {
		t.Error("GetDoc returns a doc deleted in Postgres from its stale index entry")
	}

	// Backfill complete: Postgres only.
	if err := repo.CompleteKBBackfill(ctx, rs, 1); err != nil {
		t.Fatal(err)
	}
	if docs, err := s.ListDocs(ctx, sc, ""); err != nil || len(docs) != 1 || docs[0].Path != "both.md" {
		t.Errorf("ListDocs after backfill = %+v, %v; want both.md only", docs, err)
	}
	if n, err := s.CountDocs(ctx, sc); err != nil || n != 1 {
		t.Errorf("CountDocs after backfill = %d, %v; want 1", n, err)
	}
	if _, err := s.GetDoc(ctx, sc, "old/007.md"); err == nil {
		t.Error("GetDoc after backfill still reads the index")
	}
}

// listIndexed pages through a collection bigger than one page, and a
// collection that doesn't exist yet lists empty.
func TestListIndexedPaginates(t *testing.T) {
	url, key := typesenseEnv(t)
	s := New(url, key, "", "")
	sc := Team(1_000_000 + rand.IntN(1_000_000_000))
	ctx := context.Background()
	t.Cleanup(func() { s.Drop(context.Background(), sc) }) //nolint:errcheck
	if docs, err := s.listIndexed(ctx, sc, ""); err != nil || len(docs) != 0 {
		t.Fatalf("listIndexed of a missing collection = %d, %v", len(docs), err)
	}
	if n, err := s.CountDocs(ctx, sc); err != nil || n != 0 {
		t.Fatalf("CountDocs of a missing collection = %d, %v", n, err)
	}
	for i := 0; i < 251; i++ {
		if _, err := s.indexChunks(ctx, sc, indexedDoc{path: fmt.Sprintf("%03d.md", i), title: "T", content: "c", updatedAt: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	docs, err := s.ListDocs(ctx, sc, "")
	if err != nil || len(docs) != 251 {
		t.Fatalf("ListDocs (no store) = %d, %v; want 251", len(docs), err)
	}
	if docs[0].Path != "250.md" || docs[250].Path != "000.md" {
		t.Errorf("order: first %s, last %s", docs[0].Path, docs[250].Path)
	}
}

// Dropping a stored KB (its team or org was deleted) moves every doc to the
// trash, index-only ones included, before the collection goes; reads then
// see none, and a doc can still be restored.
func TestDropTrashesStoredDocs(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "kept.md", "Kept", "in postgres", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "old.md", title: "Old", content: "index only", updatedAt: 1_700_000_000}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountDocs(ctx, sc); n != 2 {
		t.Fatalf("CountDocs before drop = %d, want 2", n)
	}
	if err := s.Drop(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountDocs(ctx, sc); err != nil || n != 0 {
		t.Errorf("CountDocs after drop = %d, %v; want 0", n, err)
	}
	deleted, err := repo.ListDeletedKBPaths(ctx, storeScope(sc))
	if err != nil || len(deleted) != 2 || deleted[0] != "kept.md" || deleted[1] != "old.md" {
		t.Errorf("trash = %v, %v; want kept.md and old.md", deleted, err)
	}
	if d, err := s.RestoreDoc(ctx, sc, "old.md", "me"); err != nil || d.Content != "index only" {
		t.Errorf("RestoreDoc after drop = %+v, %v", d, err)
	}
}
