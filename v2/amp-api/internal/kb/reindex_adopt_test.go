package kb

// A reindex of a stored KB builds from Postgres and then deletes the old
// collection, so it must first copy into Postgres what an old (pre-Postgres)
// pod wrote only to the index: an edit or annotation of a live doc, or a new
// doc, including one written while the new collection was being built
// (PR #67 review S3, AMP #2038). These need Postgres and Typesense; see
// store_test.go for the environment.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
)

// A backfilled, live doc is edited (newer updated_at, new annotation) by a
// legacy service straight in Typesense, then the KB is reindexed: first from
// its real collection (the first migration), then, after another old-pod
// annotation, alias to alias. Each time the row ends up with the edit and
// the annotation (the previous content kept in kb_document_versions) and the
// new collection serves them.
func TestReindexAdoptsOldPodEdit(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	base := collectionName(sc)
	row := annotatedDoc(t, s, repo, sc, "doc.md", "original content")
	if kc := kbCols(t, s, sc); !kc.real {
		t.Fatalf("setup: %+v, want a real collection", kc)
	}

	added := Annotation{Author: "old@x", Text: "note from the old pod", CreatedAt: row.UpdatedAt.Unix() + 1}
	oldPodWrite(t, s, sc, indexedDoc{path: "doc.md", title: "Doc v2", content: "platypusink edited by an old pod",
		author: "old@x", reason: "why", tags: []string{"a"}, annotations: append(slices.Clone(row.Annotations), added),
		updatedAt: row.UpdatedAt.Unix() + 2})

	if err := s.Reindex(ctx, sc); err != nil {
		t.Fatalf("first reindex: %v", err)
	}
	kc := kbCols(t, s, sc)
	if kc.real || !isBuildOf(base, kc.alias) || !slices.Equal(kc.builds, []string{kc.alias}) {
		t.Fatalf("after the first reindex: %+v, want only the alias and its new build", kc)
	}
	got := mustRow(t, repo, sc, "doc.md")
	wantAnns := []Annotation{row.Annotations[0], added}
	if got.Version != 2 || got.Content != "platypusink edited by an old pod" || got.Title != "Doc v2" ||
		!reflect.DeepEqual(got.Annotations, wantAnns) {
		t.Fatalf("row = v%d %q %q %+v, want the old pod's edit at v2 with both annotations", got.Version, got.Title, got.Content, got.Annotations)
	}
	vers := versionRows(t, sc, "doc.md")
	if len(vers) != 2 || vers[0] != "1|create|me|original content" ||
		vers[1] != "2|update|"+reindexEditBy+"|platypusink edited by an old pod" {
		t.Errorf("versions = %q, want the original at v1 and the edit at v2 by %s", vers, reindexEditBy)
	}
	assertServes(t, s, sc, "doc.md", got.Content, wantAnns, 2)

	// An old pod annotates it (old code keeps updated_at), now through the alias.
	added2 := Annotation{Author: "old@x", Text: "second note from the old pod", CreatedAt: got.UpdatedAt.Unix() + 30}
	oldPodWrite(t, s, sc, indexedDoc{path: "doc.md", title: got.Title, content: got.Content, author: got.Author,
		reason: got.Reason, tags: got.Tags, annotations: append(slices.Clone(got.Annotations), added2), updatedAt: got.UpdatedAt.Unix()})
	oldTarget := kc.alias

	if err := s.Reindex(ctx, sc); err != nil {
		t.Fatalf("second reindex: %v", err)
	}
	kc = kbCols(t, s, sc)
	if kc.alias == oldTarget || !slices.Equal(kc.builds, []string{kc.alias}) {
		t.Fatalf("after the second reindex: %+v, want a new target and %s dropped", kc, oldTarget)
	}
	got2 := mustRow(t, repo, sc, "doc.md")
	wantAnns = append(wantAnns, added2)
	if got2.Version != 2 || got2.Content != got.Content || !reflect.DeepEqual(got2.Annotations, wantAnns) {
		t.Fatalf("row = v%d %q %+v, want v2 unchanged plus the second note", got2.Version, got2.Content, got2.Annotations)
	}
	assertServes(t, s, sc, "doc.md", got2.Content, wantAnns, 2)
}

// assertServes checks what the KB's base name serves for path.
func assertServes(t *testing.T, s *Service, sc Scope, path, content string, anns []Annotation, version int) {
	t.Helper()
	d, err := s.getIndexed(context.Background(), sc, path)
	if err != nil {
		t.Fatalf("%s not served: %v", path, err)
	}
	if d.Content != content || !reflect.DeepEqual(d.Annotations, anns) {
		t.Errorf("%s served as %q %+v, want %q %+v", path, d.Content, d.Annotations, content, anns)
	}
	if v := indexedVersion(t, s, sc, path); v != version {
		t.Errorf("%s served kb_version %d, want %d", path, v, version)
	}
}

// A legacy service writes a new doc into the old collection while the new
// one is being built (reindex hook stage "verified", after the pre-swap
// catch-up) and, alias to alias, also straight into the old alias target
// after the swap (an in-flight write that resolved the alias before it
// moved). Each doc is copied into Postgres before the old collection is
// deleted and served by the new collection: none is lost.
func TestReindexAdoptsOldPodDocWrittenDuringIt(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "a.md", "A", "alpha", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	late := func(path string) indexedDoc {
		return indexedDoc{path: path, title: "Late " + path, content: "quokkaledger " + path, author: "old@x",
			annotations: []Annotation{{Author: "old@x", Text: "n", CreatedAt: time.Now().Unix()}}, updatedAt: time.Now().Unix()}
	}
	var oldTarget string
	round := 0
	s.reindexHook = func(hctx context.Context, stage string) {
		switch {
		case stage == "verified":
			oldPodWrite(t, s, sc, late(fmt.Sprintf("during-%d.md", round)))
		case stage == "swapped" && oldTarget != "":
			if _, err := s.indexChunksInto(hctx, sc, oldTarget, late("after-swap.md")); err != nil {
				t.Error(err)
			}
		}
	}
	defer func() { s.reindexHook = nil }()

	// First migration (real collection), then alias to alias.
	for round = 1; round <= 2; round++ {
		if err := s.Reindex(ctx, sc); err != nil {
			t.Fatalf("reindex %d: %v", round, err)
		}
		kc := kbCols(t, s, sc)
		if kc.real || !slices.Equal(kc.builds, []string{kc.alias}) {
			t.Fatalf("after reindex %d: %+v, want only the alias and its new build", round, kc)
		}
		paths := []string{fmt.Sprintf("during-%d.md", round)}
		if round == 2 {
			paths = append(paths, "after-swap.md")
		}
		for _, p := range paths {
			want := late(p)
			r := mustRow(t, repo, sc, p)
			if r.Content != want.content || len(r.Annotations) != 1 {
				t.Errorf("round %d: row of %s = %q %+v, want the old pod's doc", round, p, r.Content, r.Annotations)
			}
			if by := versionRows(t, sc, p); len(by) != 1 || by[0] != "1|backfill|"+reindexAdoptBy+"|"+want.content {
				t.Errorf("round %d: versions of %s = %q, want one insert-only copy by %s", round, p, by, reindexAdoptBy)
			}
			assertServes(t, s, sc, p, want.content, r.Annotations, 1)
		}
		oldTarget = kc.alias
	}
}

// An entry with no row that an old pod can't have written after the
// backfill (dated long before it) appears in the old collection during the
// build. The reindex aborts before the swap: the old collection keeps
// serving, the entry included, and the new build is dropped.
func TestReindexAbortsOnUnadoptableEntryDuringIt(t *testing.T) {
	s, _, sc := backfilledKB(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "a.md", "A", "alpha", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Reindex(ctx, sc); err != nil { // to an alias
		t.Fatal(err)
	}
	before := kbCols(t, s, sc)
	s.reindexHook = func(_ context.Context, stage string) {
		if stage == "verified" {
			oldPodWrite(t, s, sc, indexedDoc{path: "ancient.md", title: "Ancient", content: "old", author: "x", updatedAt: 1})
		}
	}
	defer func() { s.reindexHook = nil }()

	err := s.Reindex(ctx, sc)
	if !errors.Is(err, ErrReindexRefused) {
		t.Fatalf("reindex returned %v, want ErrReindexRefused", err)
	}
	if kc := kbCols(t, s, sc); kc.alias != before.alias || !slices.Equal(kc.builds, before.builds) {
		t.Fatalf("after the abort: %+v, want %+v (old collection serving, new build dropped)", kc, before)
	}
	if _, err := s.getIndexed(ctx, sc, "ancient.md"); err != nil {
		t.Fatalf("the entry is gone: %v", err)
	}
}
