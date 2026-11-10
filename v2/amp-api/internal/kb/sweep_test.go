package kb

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The sweep removes only the collections under the test prefix it is given —
// not another run's, and never anything else — so a suite run against the
// Typesense a live instance uses leaves that instance as it found it (#3689).
// Needs Typesense.
//
// Every collection this test creates is namespaced. It deliberately never
// creates a live-named collection to prove the sweep spares one: writing
// kb_global_0 (or any live name) and dropping it is the very defect #3689 is
// about. The live names are protected by the refusal below, which is the
// actual barrier.
func TestDropTestCollections(t *testing.T) {
	url, key := typesenseEnv(t)
	ctx := context.Background()
	sfx := time.Now().UnixNano() % 1_000_000_000

	mine := New(url, key, "", "")
	mine.SetCollectionPrefix(fmt.Sprintf("%ssweep_%d_", testCollectionPrefix, sfx))
	ours := []Scope{Project(int(sfx)), Global()}
	for _, sc := range ours {
		if _, err := mine.WriteDoc(ctx, sc, "a.md", "A", "alpha", "me", nil, "", nil); err != nil {
			t.Fatalf("write %s: %v", sc, err)
		}
	}

	// Another run's namespace, which this sweep must leave alone.
	other := New(url, key, "", "")
	other.SetCollectionPrefix(fmt.Sprintf("%sother_%d_", testCollectionPrefix, sfx))
	if _, err := other.WriteDoc(ctx, Project(7), "b.md", "B", "beta", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.DropTestCollections(context.Background(), other.collectionPrefix) })

	if err := mine.DropTestCollections(ctx, mine.collectionPrefix); err != nil {
		t.Fatalf("sweep %q: %v", mine.collectionPrefix, err)
	}

	// This run's collections are gone...
	for _, sc := range ours {
		if _, err := mine.client.Collection(mine.CollectionName(sc)).Retrieve(ctx); err == nil {
			t.Errorf("%s survived the sweep (%s)", sc, mine.CollectionName(sc))
		}
	}
	// ...the other run's is not.
	if n, err := other.CountDocs(ctx, Project(7)); err != nil || n != 1 {
		t.Errorf("another run's collection after the sweep: %d docs, err %v", n, err)
	}
}

// A prefix that isn't a test namespace is refused before Typesense is
// touched, so the sweep can never be pointed at live knowledge — above all
// kb_global_0, whose guide a live instance syncs only at startup. Needs no
// Typesense: the refusal happens first.
func TestDropTestCollectionsRefusesLiveNames(t *testing.T) {
	s := New("http://127.0.0.1:1", "k", "", "")
	for _, bad := range []string{"", "kb_", "kbtest", "_kbtest_", collectionName(Global()), collectionName(Project(5)), collectionName(Team(5))} {
		err := s.DropTestCollections(context.Background(), bad)
		if err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("sweep of %q: err = %v, want a refusal", bad, err)
		}
	}
}
