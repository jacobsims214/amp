package kb

// Tests for the startup backfill from Typesense into Postgres (#1994). They
// need Postgres and Typesense; see store_test.go for the environment. The
// test DB and Typesense are shared, so every BackfillAll here is limited to
// the test's own KBs (backfillOnly).

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/typesense/typesense-go/v4/typesense/api"

	"github.com/simstech/amp-api/internal/repository"
)

// backfillKB returns a stored service and a fresh project KB that no
// projects row owns (an orphan collection, or an archived project: the
// backfill finds both from Typesense alone). Its collections are dropped
// after the test.
func backfillKB(t *testing.T) (*Service, *repository.Repo, Scope) {
	t.Helper()
	repo := storeTestRepo(t)
	url, key := typesenseEnv(t)
	s := New(url, key, "", "")
	s.SetStore(repo)
	sc := Project(1_000_000 + rand.IntN(1_000_000_000))
	s.backfillOnly = func(x Scope) bool { return x == sc }
	t.Cleanup(func() { s.dropCollections(context.Background(), collectionName(sc)) }) //nolint:errcheck
	return s, repo, sc
}

func fastBackfillRetries(t *testing.T) {
	t.Helper()
	oldWait, oldPoll := backfillRetryWait, backfillLockPoll
	backfillRetryWait, backfillLockPoll = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { backfillRetryWait, backfillLockPoll = oldWait, oldPoll })
}

type rowSnap struct {
	id        int64
	version   int
	updatedAt time.Time
	content   string
	anns      int
	deleted   bool
}

func snapshotRows(t *testing.T, repo *repository.Repo, sc Scope) map[string]rowSnap {
	t.Helper()
	out := map[string]rowSnap{}
	err := repo.StreamKBDocuments(context.Background(), storeScope(sc), func(d repository.KBDocument) error {
		out[d.Path] = rowSnap{d.ID, d.Version, d.UpdatedAt, d.Content, len(d.Annotations), false}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	del, err := repo.ListDeletedKBPaths(context.Background(), storeScope(sc))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range del {
		out[p] = rowSnap{deleted: true}
	}
	return out
}

func backfillState(t *testing.T, repo *repository.Repo, sc Scope) *repository.KBBackfill {
	t.Helper()
	b, err := repo.GetKBBackfill(context.Background(), storeScope(sc))
	if err != nil {
		t.Fatalf("kb_backfills row of %s: %v", sc, err)
	}
	return b
}

// A KB of more than 250 docs (past the old one-page cap) is copied exactly:
// every doc, with its own updated_at, content (multi-chunk docs whole),
// author, reason, tags and annotations; the KB is recorded complete with
// its doc count. A second run changes nothing.
func TestBackfillCopiesLargeKBExactly(t *testing.T) {
	s, repo, sc := backfillKB(t)
	ctx := context.Background()
	const n = 263
	base := int64(1_600_000_000)
	want := map[string]indexedDoc{}
	for i := 0; i < n; i++ {
		d := indexedDoc{
			path: fmt.Sprintf("notes/%03d.md", i), title: fmt.Sprintf("Note %d", i),
			content: fmt.Sprintf("body %d", i), author: fmt.Sprintf("a%d@x", i%7), reason: fmt.Sprintf("r%d", i%3),
			tags: []string{"t", fmt.Sprintf("g%d", i%5)}, updatedAt: base + int64(i)*3600,
		}
		if i%10 == 0 {
			d.annotations = []Annotation{
				{Author: "rev", Text: fmt.Sprintf("note on %d", i), CreatedAt: base + 7},
				{Author: "rev2", Text: "fixed", CreatedAt: base + 8, IsResolved: true, ResolvedBy: "me", ResolvedAt: base + 9},
			}
		}
		if i == 5 {
			d.content = longDoc(4, "multichunk") // several chunks; only chunk 0 counts
		}
		if _, err := s.indexChunks(ctx, sc, d); err != nil {
			t.Fatal(err)
		}
		want[d.path] = d
	}
	if c := chunkCount(t, s, sc, "notes/005.md"); c < 3 {
		t.Fatalf("notes/005.md has %d chunks, want several", c)
	}
	if done, _ := repo.IsKBBackfillComplete(ctx, storeScope(sc)); done {
		t.Fatal("KB is complete before the backfill")
	}

	stats, err := s.BackfillAll(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 {
		t.Fatalf("backfilled %d KBs, want 1 (the orphan project collection found in Typesense): %+v", len(stats), stats)
	}
	st := stats[0]
	if st.Skipped != "" || st.Docs != n || st.Inserted != n || st.Present != 0 || len(st.Collections) != 1 ||
		st.Collections[0].Found != n || st.Collections[0].Inserted != n || st.Collections[0].Collection != collectionName(sc) {
		t.Fatalf("stats = %+v", st)
	}
	if c, err := repo.CountKBDocuments(ctx, storeScope(sc)); err != nil || c != n {
		t.Fatalf("postgres holds %d docs, %v; want %d", c, err, n)
	}
	b := backfillState(t, repo, sc)
	if b.CompletedAt == nil || b.DocCount != n || b.LastError != "" {
		t.Fatalf("kb_backfills = %+v", b)
	}
	for path, w := range want {
		row := mustRow(t, repo, sc, path)
		if row.Title != w.title || row.Content != w.content || row.Author != w.author || row.Reason != w.reason ||
			fmt.Sprint(row.Tags) != fmt.Sprint(w.tags) || row.Version != 1 {
			t.Errorf("%s: row = %+v", path, row)
		}
		if row.UpdatedAt.Unix() != w.updatedAt || row.CreatedAt.Unix() != w.updatedAt {
			t.Errorf("%s: dates %v / %v, want %d", path, row.CreatedAt, row.UpdatedAt, w.updatedAt)
		}
		if fmt.Sprintf("%+v", row.Annotations) != fmt.Sprintf("%+v", kbAnnotationsOrEmpty(w.annotations)) {
			t.Errorf("%s: annotations %+v, want %+v", path, row.Annotations, w.annotations)
		}
	}
	// Reads now come from Postgres alone and match.
	if c, err := s.CountDocs(ctx, sc); err != nil || c != n {
		t.Errorf("CountDocs = %d, %v", c, err)
	}

	// Second run: a no-op.
	before := snapshotRows(t, repo, sc)
	stats, err = s.BackfillAll(ctx, nil)
	if err != nil || len(stats) != 1 || stats[0].Skipped != "already complete" || stats[0].Inserted != 0 {
		t.Fatalf("second run = %+v, %v", stats, err)
	}
	// Even forcing the copy again inserts nothing and changes no row.
	again, err := s.backfillOnce(ctx, sc)
	if err != nil || again.Inserted != 0 || again.Present != n {
		t.Fatalf("forced re-copy = %+v, %v", again, err)
	}
	after := snapshotRows(t, repo, sc)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Error("a second run changed postgres rows")
	}
	if b2 := backfillState(t, repo, sc); !b2.CompletedAt.Equal(*b.CompletedAt) {
		t.Errorf("a second run re-recorded completion: %v then %v", b.CompletedAt, b2.CompletedAt)
	}
}

// Insert-only: a path whose row was soft-deleted stays deleted (the stale
// index entry doesn't revive it), and a path with a newer live row keeps
// it; only the index-only doc is inserted.
func TestBackfillNeverRevivesOrOverwrites(t *testing.T) {
	s, repo, sc := backfillKB(t)
	ctx := context.Background()
	rs := storeScope(sc)
	for _, d := range []indexedDoc{
		{path: "deleted.md", title: "Deleted", content: "old deleted", updatedAt: 1_600_000_000},
		{path: "newer.md", title: "Newer", content: "old index copy", updatedAt: 1_600_000_100},
		{path: "only-index.md", title: "Only", content: "only in the index", updatedAt: 1_600_000_200},
	} {
		if _, err := s.indexChunks(ctx, sc, d); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"deleted.md", "newer.md"} {
		if _, err := repo.InsertKBDocumentIfMissing(ctx, rs, repository.KBDocument{Path: p, Title: p, Content: "postgres " + p}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.DeleteKBDocument(ctx, rs, "deleted.md", "me"); err != nil {
		t.Fatal(err)
	}

	st, err := s.BackfillKB(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if st.Docs != 3 || st.Inserted != 1 || st.Present != 2 {
		t.Fatalf("stats = %+v; want 3 found, 1 inserted, 2 present", st)
	}
	if _, err := repo.GetKBDocument(ctx, rs, "deleted.md"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("deleted.md was revived: %v", err)
	}
	if del, _ := repo.ListDeletedKBPaths(ctx, rs); len(del) != 1 || del[0] != "deleted.md" {
		t.Errorf("deleted paths = %v", del)
	}
	if row := mustRow(t, repo, sc, "newer.md"); row.Content != "postgres newer.md" {
		t.Errorf("newer.md was overwritten: %+v", row)
	}
	if row := mustRow(t, repo, sc, "only-index.md"); row.Content != "only in the index" || row.UpdatedAt.Unix() != 1_600_000_200 {
		t.Errorf("only-index.md = %+v", row)
	}
	if b := backfillState(t, repo, sc); b.CompletedAt == nil || b.DocCount != 3 {
		t.Errorf("kb_backfills = %+v", b)
	}
	// Reads after completion don't bring the deleted doc back either.
	if _, err := s.GetDoc(ctx, sc, "deleted.md"); err == nil {
		t.Error("GetDoc returned the deleted doc")
	}
}

// A KB with no search collection has nothing to copy: the run marks the KBs
// Postgres knows of complete, and a KB first touched after startup (a new
// project) is marked complete by its first read or write. No collection is
// created.
func TestBackfillMarksKBWithoutCollectionComplete(t *testing.T) {
	s, repo, sc := backfillKB(t)
	ctx := context.Background()
	owners := func(context.Context) ([]repository.KBScope, error) {
		return []repository.KBScope{storeScope(sc)}, nil
	}
	stats, err := s.BackfillAll(ctx, owners)
	if err != nil || len(stats) != 1 || stats[0].Skipped != "" || stats[0].Docs != 0 || len(stats[0].Collections) != 0 {
		t.Fatalf("run = %+v, %v", stats, err)
	}
	if b := backfillState(t, repo, sc); b.CompletedAt == nil || b.DocCount != 0 {
		t.Fatalf("kb_backfills = %+v", b)
	}
	if kc, err := s.kbCollections(ctx, collectionName(sc)); err != nil || kc.real || kc.alias != "" {
		t.Errorf("collections after the run = %+v, %v; want none", kc, err)
	}

	// Lazily: a new KB's first read, and another's first write.
	for i, touch := range []func(Scope) error{
		func(x Scope) error { _, err := s.ListDocs(ctx, x, ""); return err },
		func(x Scope) error {
			_, err := s.WriteDoc(ctx, x, "first.md", "First", "first doc", "me", nil, "", nil)
			return err
		},
	} {
		x := Team(1_000_000 + rand.IntN(1_000_000_000))
		t.Cleanup(func() { s.Drop(context.Background(), x) }) //nolint:errcheck
		if err := touch(x); err != nil {
			t.Fatal(err)
		}
		if done, err := repo.IsKBBackfillComplete(ctx, storeScope(x)); err != nil || !done {
			t.Errorf("case %d: new KB complete = %v, %v; want true", i, done, err)
		}
	}
	// A KB that has a collection is not marked by a read.
	y := Team(1_000_000 + rand.IntN(1_000_000_000))
	t.Cleanup(func() { s.Drop(context.Background(), y) }) //nolint:errcheck
	if _, err := s.indexChunks(ctx, y, indexedDoc{path: "pre.md", title: "Pre", content: "pre", updatedAt: 1_600_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListDocs(ctx, y, ""); err != nil {
		t.Fatal(err)
	}
	if done, _ := repo.IsKBBackfillComplete(ctx, storeScope(y)); done {
		t.Error("a KB with a collection was marked complete without a backfill")
	}
}

// A count mismatch, an unreadable doc, or an unreachable Typesense records
// the KB failed, not complete; the KB keeps its index-merging reads, and a
// later run (the next startup) can still complete it.
func TestBackfillMismatchMarksFailed(t *testing.T) {
	fastBackfillRetries(t)
	ctx := context.Background()

	t.Run("count mismatch", func(t *testing.T) {
		s, repo, sc := backfillKB(t)
		if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "a.md", title: "A", content: "a", updatedAt: 1_600_000_000}); err != nil {
			t.Fatal(err)
		}
		// A second chunk-0 record for the same path under another id: the
		// export has 2 records but only 1 distinct path.
		dup := map[string]interface{}{
			"id": "dup-a", "project_id": sc.ID, "path": "a.md", "title": "A dup", "tags": []string{}, "author": "",
			"reason": "", "chunk_index": 0, "chunk_text": "a", "content": "a dup", "updated_at": 1_600_000_001, "annotations": "[]",
		}
		if _, err := s.client.Collection(collectionName(sc)).Documents().Upsert(ctx, dup, &api.DocumentIndexParameters{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BackfillKB(ctx, sc); err == nil || !errors.Is(err, errBackfillUnstable) {
			t.Fatalf("BackfillKB = %v; want a count mismatch", err)
		}
		b := backfillState(t, repo, sc)
		if b.CompletedAt != nil || b.LastError == "" {
			t.Errorf("kb_backfills = %+v; want failed, not complete", b)
		}
		// Fixed (the duplicate removed): the next run completes it.
		if _, err := s.client.Collection(collectionName(sc)).Document("dup-a").Delete(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BackfillKB(ctx, sc); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if b := backfillState(t, repo, sc); b.CompletedAt == nil || b.LastError != "" || b.DocCount != 1 {
			t.Errorf("after the retry kb_backfills = %+v", b)
		}
	})

	t.Run("unreadable annotations", func(t *testing.T) {
		s, repo, sc := backfillKB(t)
		if err := s.ensureCollection(ctx, sc); err != nil {
			t.Fatal(err)
		}
		bad := map[string]interface{}{
			"id": docID(sc, "bad.md", 0), "project_id": sc.ID, "path": "bad.md", "title": "Bad", "tags": []string{},
			"author": "", "reason": "", "chunk_index": 0, "chunk_text": "b", "content": "b", "updated_at": 1_600_000_000,
			"annotations": "{not json",
		}
		if _, err := s.client.Collection(collectionName(sc)).Documents().Upsert(ctx, bad, &api.DocumentIndexParameters{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BackfillKB(ctx, sc); err == nil {
			t.Fatal("BackfillKB copied a doc whose annotations it couldn't read")
		}
		if b := backfillState(t, repo, sc); b.CompletedAt != nil || b.LastError == "" {
			t.Errorf("kb_backfills = %+v", b)
		}
		if _, err := repo.GetKBDocument(ctx, storeScope(sc), "bad.md"); !errors.Is(err, repository.ErrNotFound) {
			t.Errorf("bad.md was copied without its annotations: %v", err)
		}
	})

	t.Run("typesense down", func(t *testing.T) {
		repo := storeTestRepo(t)
		s := New("http://127.0.0.1:1", "x", "", "")
		s.SetStore(repo)
		sc := Project(1_000_000 + rand.IntN(1_000_000_000))
		if _, err := s.BackfillKB(ctx, sc); err == nil {
			t.Fatal("BackfillKB with typesense down succeeded")
		}
		if b := backfillState(t, repo, sc); b.CompletedAt != nil || b.LastError == "" {
			t.Errorf("kb_backfills = %+v", b)
		}
		// And the whole run fails without marking anything.
		s.backfillOnly = func(x Scope) bool { return x == sc }
		if _, err := s.BackfillAll(ctx, func(context.Context) ([]repository.KBScope, error) {
			return []repository.KBScope{storeScope(sc)}, nil
		}); err == nil {
			t.Error("BackfillAll with typesense down succeeded")
		}
		if done, _ := repo.IsKBBackfillComplete(ctx, storeScope(sc)); done {
			t.Error("KB marked complete while typesense was down")
		}
	})
}

// Reindex refuses, and the reconciler skips, a KB with a collection until
// its backfill is recorded complete; afterwards both run.
func TestReindexAndReconcileWaitForBackfill(t *testing.T) {
	s, repo, sc := backfillKB(t)
	ctx := context.Background()
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "pre.md", title: "Pre", content: "pre-existing", updatedAt: 1_600_000_000}); err != nil {
		t.Fatal(err)
	}
	err := s.Reindex(ctx, sc)
	if !errors.Is(err, ErrReindexRefused) || !errors.Is(err, ErrBackfillPending) {
		t.Fatalf("reindex before the backfill = %v; want ErrReindexRefused and ErrBackfillPending", err)
	}
	if kc, _ := s.kbCollections(ctx, collectionName(sc)); !kc.real || kc.alias != "" || len(kc.builds) != 0 {
		t.Errorf("the refused reindex changed the collections: %+v", kc)
	}
	if _, err := repo.GetKBDocument(ctx, storeScope(sc), "pre.md"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("the refused reindex wrote postgres: %v", err)
	}
	rec := reconciler(t, repo)
	if st, err := rec.ReconcileKB(ctx, sc); err != nil || st.Skipped != "backfill not complete" {
		t.Errorf("reconcile before the backfill = %+v, %v", st, err)
	}

	if _, err := s.BackfillKB(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if st, err := rec.ReconcileKB(ctx, sc); err != nil || st.Skipped != "" || st.Live != 1 || st.Indexed != 1 {
		t.Errorf("reconcile after the backfill = %+v, %v", st, err)
	}
	if err := s.Reindex(ctx, sc); err != nil {
		t.Fatalf("reindex after the backfill: %v", err)
	}
	doc, err := s.getIndexed(ctx, sc, "pre.md")
	if err != nil || doc.Content != "pre-existing" || doc.UpdatedAt != 1_600_000_000 {
		t.Errorf("after reindex the index has %+v, %v", doc, err)
	}
}

// A KB already migrated to an alias is read through the alias target; a
// leftover build no alias points at is ignored; the run takes the
// cross-replica lock, and while another replica holds it does nothing.
func TestBackfillReadsThroughAliasAndTakesTheLock(t *testing.T) {
	s, repo, sc := backfillKB(t)
	ctx := context.Background()
	base := collectionName(sc)
	target, stale := base+"_v1", base+"_v2"
	for _, col := range []string{target, stale} {
		if _, err := s.client.Collections().Create(ctx, s.collectionSchema(ctx, col)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := s.indexChunksInto(ctx, sc, target, indexedDoc{path: fmt.Sprintf("%d.md", i), title: "T", content: "via alias", updatedAt: 1_600_000_000 + int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.indexChunksInto(ctx, sc, stale, indexedDoc{path: "purged.md", title: "P", content: "old build", updatedAt: 1_500_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.client.Aliases().Upsert(ctx, base, &api.CollectionAliasSchema{CollectionName: target}); err != nil {
		t.Fatal(err)
	}

	scopes, err := s.discoverKBScopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range scopes {
		found = found || x == sc
		if x.Kind == ScopeGlobal || (x.Kind == ScopeProject && x.ID == 0) {
			t.Errorf("discovered %v", x)
		}
	}
	if !found {
		t.Fatalf("the aliased KB was not discovered")
	}

	// Another replica holds the backfill lock: this one does nothing.
	release, ok, err := repo.TryNamedLock(ctx, backfillLockKey)
	if err != nil || !ok {
		t.Fatalf("take the lock: %v, %v", ok, err)
	}
	s.RunBackfill(ctx, nil, repo.TryNamedLock)
	if done, _ := repo.IsKBBackfillComplete(ctx, storeScope(sc)); done {
		t.Fatal("ran while another replica held the backfill lock")
	}
	release()
	s.RunBackfill(ctx, nil, repo.TryNamedLock)

	b := backfillState(t, repo, sc)
	if b.CompletedAt == nil || b.DocCount != 3 {
		t.Fatalf("kb_backfills = %+v", b)
	}
	if c, _ := repo.CountKBDocuments(ctx, storeScope(sc)); c != 3 {
		t.Errorf("postgres holds %d docs, want 3", c)
	}
	if _, err := repo.GetKBDocument(ctx, storeScope(sc), "purged.md"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("a doc from a build no alias points at was copied: %v", err)
	}
}

func TestScopeOfBase(t *testing.T) {
	tests := []struct {
		name string
		want Scope
		ok   bool
	}{
		{"kb_5", Project(5), true},
		{"kb_team_7", Team(7), true},
		{"kb_org_9", Org(9), true},
		{"kb_global_0", Scope{}, false},
		{"kb_5_v123", Scope{}, false},
		{"kb_team_x", Scope{}, false},
		{"users", Scope{}, false},
	}
	for _, tt := range tests {
		got, ok := scopeOfBase(tt.name)
		if ok != tt.ok || got != tt.want {
			t.Errorf("scopeOfBase(%q) = %v, %v; want %v, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
	for name, want := range map[string]string{"kb_5_v1": "kb_5", "kb_team_7_v99": "kb_team_7", "kb_global_0_v1": ""} {
		m := buildName.FindStringSubmatch(name)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("build %q -> base %q, want %q", name, got, want)
		}
	}
}
