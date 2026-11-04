package kb

// Tests for the build-then-swap reindex of stored KBs (#1990). They need
// Postgres and Typesense; see store_test.go for the environment.

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/repository"
)

func kbCols(t *testing.T, s *Service, sc Scope) kbCollections {
	t.Helper()
	kc, err := s.kbCollections(context.Background(), collectionName(sc))
	if err != nil {
		t.Fatal(err)
	}
	return kc
}

func mustCount(t *testing.T, s *Service, sc Scope) int {
	t.Helper()
	n, err := s.countIndexed(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The first reindex of a KB served by a real collection named kb_... builds
// a new collection, replaces the real one with an alias of the same name,
// and keeps every doc (with its updated_at and annotations). Search, writes,
// a second reindex (alias to alias) and Drop all go through the alias.
func TestReindexMigratesCollectionToAlias(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		if _, err := s.WriteDoc(ctx, sc, p, "Title "+p, "Zebrafish notes for "+p, "me", []string{"x"}, "", nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // distinct updated_at per doc
	}
	if _, err := s.AnnotateDoc(ctx, sc, "b.md", "note", "me"); err != nil {
		t.Fatal(err)
	}
	base := collectionName(sc)
	if kc := kbCols(t, s, sc); !kc.real || kc.alias != "" || len(kc.builds) != 0 {
		t.Fatalf("before: %+v, want a real collection and no alias", kc)
	}

	if err := s.reindex(ctx, sc, false); err != nil {
		t.Fatal(err)
	}
	kc := kbCols(t, s, sc)
	if kc.real || !isBuildOf(base, kc.alias) || !slices.Equal(kc.builds, []string{kc.alias}) {
		t.Fatalf("after: %+v, want only an alias %s -> %s_v<n>", kc, base, base)
	}
	first := kc.alias
	if n := mustCount(t, s, sc); n != 3 {
		t.Fatalf("indexed %d docs through the alias, want 3", n)
	}
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		row := mustRow(t, repo, sc, p)
		doc, err := s.getIndexed(ctx, sc, p)
		if err != nil {
			t.Fatal(err)
		}
		if doc.UpdatedAt != row.UpdatedAt.Unix() {
			t.Errorf("%s: indexed updated_at %d, postgres %d", p, doc.UpdatedAt, row.UpdatedAt.Unix())
		}
		if doc.Content != row.Content || len(doc.Annotations) != len(row.Annotations) {
			t.Errorf("%s: indexed content/annotations differ from postgres", p)
		}
	}
	hits, err := s.Search(ctx, sc, "zebrafish", nil, 10, 0, 0)
	if err != nil || len(hits) != 3 {
		t.Fatalf("search through the alias: %d hits, err %v", len(hits), err)
	}

	// A write after the swap lands in the aliased collection.
	if _, err := s.WriteDoc(ctx, sc, "d.md", "D", "Zebrafish d", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if n, err := s.countIn(ctx, first); err != nil || n != 4 {
		t.Fatalf("%s holds %d docs (err %v), want 4", first, n, err)
	}
	if kc := kbCols(t, s, sc); kc.real {
		t.Fatalf("a write recreated a real collection %s that shadows the alias", base)
	}

	// Alias to alias: the old build is dropped once the new one serves.
	if err := s.reindex(ctx, sc, false); err != nil {
		t.Fatal(err)
	}
	kc = kbCols(t, s, sc)
	if kc.alias == first || !slices.Equal(kc.builds, []string{kc.alias}) {
		t.Fatalf("after second reindex: %+v, want a new alias target and %s dropped", kc, first)
	}
	if n := mustCount(t, s, sc); n != 4 {
		t.Fatalf("indexed %d docs after second reindex, want 4", n)
	}

	if err := s.Drop(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if kc := kbCols(t, s, sc); kc.real || kc.alias != "" || len(kc.builds) != 0 {
		t.Fatalf("after Drop: %+v, want nothing left", kc)
	}
}

// A reindex cancelled before the swap leaves the serving collection
// unchanged and drops only its new build, both before the first migration
// (a real collection serves) and after it (an alias serves).
func TestReindexCancelBeforeSwapKeepsOld(t *testing.T) {
	s, _, sc := storedService(t)
	bg := context.Background()
	for _, p := range []string{"a.md", "b.md"} {
		if _, err := s.WriteDoc(bg, sc, p, p, "content "+p, "me", nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	cancelled := func() error {
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		s.reindexHook = func(_ context.Context, stage string) {
			if stage == "verified" {
				cancel()
			}
		}
		defer func() { s.reindexHook = nil }()
		return s.reindex(ctx, sc, false)
	}

	if err := cancelled(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reindex returned %v, want context.Canceled", err)
	}
	if kc := kbCols(t, s, sc); !kc.real || kc.alias != "" || len(kc.builds) != 0 {
		t.Fatalf("after cancel (real collection): %+v, want the real collection only", kc)
	}
	if n := mustCount(t, s, sc); n != 2 {
		t.Fatalf("old collection holds %d docs, want 2", n)
	}

	if err := s.reindex(bg, sc, false); err != nil {
		t.Fatal(err)
	}
	serving := kbCols(t, s, sc).alias
	if err := cancelled(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reindex returned %v, want context.Canceled", err)
	}
	if kc := kbCols(t, s, sc); kc.alias != serving || !slices.Equal(kc.builds, []string{serving}) {
		t.Fatalf("after cancel (alias): %+v, want alias still -> %s and no other build", kc, serving)
	}
	if doc, err := s.getIndexed(bg, sc, "a.md"); err != nil || doc.Content != "content a.md" {
		t.Fatalf("old collection lost a.md: %v", err)
	}
}

// A reindex refuses, changing nothing, when the index holds a doc Postgres
// has no row for (Postgres holds fewer docs than the index).
func TestReindexRefusesWhenIndexHasMore(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "a.md", "A", "Alpha", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteKBBackfill(ctx, storeScope(sc), 1); err != nil {
		t.Fatal(err)
	}
	// In the index only, e.g. written by an old pod after the backfill.
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "only-indexed.md", title: "O", content: "Only here", author: "old", updatedAt: 1}); err != nil {
		t.Fatal(err)
	}

	err := s.reindex(ctx, sc, true)
	if !errors.Is(err, ErrReindexRefused) {
		t.Fatalf("reindex returned %v, want ErrReindexRefused", err)
	}
	if kc := kbCols(t, s, sc); !kc.real || kc.alias != "" || len(kc.builds) != 0 {
		t.Fatalf("after refusal: %+v, want the original collection untouched", kc)
	}
	if n := mustCount(t, s, sc); n != 2 {
		t.Fatalf("index holds %d docs after refusal, want 2", n)
	}
	if _, err := s.getIndexed(ctx, sc, "only-indexed.md"); err != nil {
		t.Fatalf("the index-only doc is gone: %v", err)
	}
}

// Writes made while a reindex runs end up in the new collection: index jobs
// that wrote into the old collection during the build (write, annotate,
// delete, new doc) are caught up before the swap, and writes committed just
// before and just after the swap whose index jobs went to the old
// collection, or haven't run, are caught up after it.
func TestReindexKeepsWritesMadeDuringIt(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	rs := storeScope(sc)
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		if _, err := s.WriteDoc(ctx, sc, p, p, "v1 "+p, "me", nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	commitOnly := func(path, content string) {
		if _, _, err := repo.UpsertKBDocument(ctx, rs, repository.KBDocWrite{Path: path, Title: path, Content: content, Author: "me", By: "me"}); err != nil {
			t.Fatal(err)
		}
	}
	s.reindexHook = func(hctx context.Context, stage string) {
		switch stage {
		case "built":
			// Indexed synchronously into whatever kb_... resolves to: the old collection.
			if _, err := s.WriteDoc(hctx, sc, "a.md", "a.md", "v2 a.md", "me", nil, "", nil); err != nil {
				t.Error(err)
			}
			if _, err := s.AnnotateDoc(hctx, sc, "b.md", "during reindex", "me"); err != nil {
				t.Error(err)
			}
			if err := s.DeleteDoc(hctx, sc, "c.md", "me"); err != nil {
				t.Error(err)
			}
			if _, err := s.WriteDoc(hctx, sc, "d.md", "d.md", "v1 d.md", "me", nil, "", nil); err != nil {
				t.Error(err)
			}
		case "verified":
			// Committed after the pre-swap catch-up; its index job wrote into the old collection.
			commitOnly("e.md", "v1 e.md")
			if err := s.indexFromStore(hctx, sc, "e.md"); err != nil {
				t.Error(err)
			}
		case "swapped":
			// Committed, index job not run yet.
			commitOnly("f.md", "v1 f.md")
		}
	}
	if err := s.reindex(ctx, sc, false); err != nil {
		t.Fatal(err)
	}
	s.reindexHook = nil

	kc := kbCols(t, s, sc)
	if kc.real || !isBuildOf(collectionName(sc), kc.alias) {
		t.Fatalf("after reindex: %+v, want the alias serving a new build", kc)
	}
	want := map[string]string{"a.md": "v2 a.md", "b.md": "v1 b.md", "d.md": "v1 d.md", "e.md": "v1 e.md", "f.md": "v1 f.md"}
	for p, content := range want {
		doc, err := s.getIndexed(ctx, sc, p)
		if err != nil {
			t.Errorf("%s missing from the new collection: %v", p, err)
			continue
		}
		if doc.Content != content {
			t.Errorf("%s: indexed %q, want %q", p, doc.Content, content)
		}
		if row := mustRow(t, repo, sc, p); doc.UpdatedAt != row.UpdatedAt.Unix() {
			t.Errorf("%s: indexed updated_at %d, postgres %d", p, doc.UpdatedAt, row.UpdatedAt.Unix())
		}
	}
	if doc, err := s.getIndexed(ctx, sc, "b.md"); err == nil && len(doc.Annotations) != 1 {
		t.Errorf("b.md has %d annotations in the new collection, want 1", len(doc.Annotations))
	}
	if _, err := s.getIndexed(ctx, sc, "c.md"); err == nil {
		t.Error("c.md was deleted during the reindex but is in the new collection")
	}
	if n := mustCount(t, s, sc); n != len(want) {
		t.Errorf("new collection holds %d docs, want %d", n, len(want))
	}
}
