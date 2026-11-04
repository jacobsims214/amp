package kb

// Tests for the reconciler (#1993). They need Postgres and Typesense; see
// store_test.go for the environment. Every reconcile here runs on a
// service whose store refuses every write except InsertKBDocumentIfMissing
// (adopting an index-only doc, #2024), so each test also proves the
// reconciler never updates or deletes a Postgres row. The one exception is
// copying an old pod's edit of a live row into it (#2027): only the tests of
// that case set allowEdits, which lets UpsertKBDocument and
// UpdateKBAnnotations through and counts them.

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// insertOnlyStore is a Store that fails the test on any write other than
// InsertKBDocumentIfMissing, which it passes through and counts. With
// allowEdits it also passes UpsertKBDocument and UpdateKBAnnotations through
// (counted), for the old-pod-edit tests only.
type insertOnlyStore struct {
	Store
	t          *testing.T
	allowEdits bool
	writes     atomic.Int32 // refused writes
	inserts    atomic.Int32 // InsertKBDocumentIfMissing calls that inserted
	upserts    atomic.Int32 // UpsertKBDocument calls (allowEdits)
	annUpdates atomic.Int32 // UpdateKBAnnotations calls (allowEdits)
}

func (r *insertOnlyStore) wrote(name string) error {
	r.writes.Add(1)
	r.t.Errorf("reconciler wrote postgres: %s", name)
	return fmt.Errorf("read-only store: %s refused", name)
}

func (r *insertOnlyStore) UpsertKBDocument(ctx context.Context, sc repository.KBScope, w repository.KBDocWrite) (*repository.KBDocument, bool, error) {
	if !r.allowEdits {
		return nil, false, r.wrote("UpsertKBDocument")
	}
	r.upserts.Add(1)
	if w.By != reconcileEditBy {
		r.t.Errorf("UpsertKBDocument by %q, want %q", w.By, reconcileEditBy)
	}
	return r.Store.UpsertKBDocument(ctx, sc, w)
}

func (r *insertOnlyStore) UpdateKBAnnotations(ctx context.Context, sc repository.KBScope, path string,
	fn func([]domain.KBAnnotation) ([]domain.KBAnnotation, error)) (*repository.KBDocument, error) {
	if !r.allowEdits {
		return nil, r.wrote("UpdateKBAnnotations")
	}
	r.annUpdates.Add(1)
	return r.Store.UpdateKBAnnotations(ctx, sc, path, fn)
}

// InsertKBDocumentIfMissing is the one write the reconciler may make: it
// never changes an existing row, live or deleted.
func (r *insertOnlyStore) InsertKBDocumentIfMissing(ctx context.Context, sc repository.KBScope, d repository.KBDocument, by string) (bool, error) {
	ok, err := r.Store.InsertKBDocumentIfMissing(ctx, sc, d, by)
	if ok {
		r.inserts.Add(1)
	}
	return ok, err
}

func (r *insertOnlyStore) DeleteKBDocument(context.Context, repository.KBScope, string, string) error {
	return r.wrote("DeleteKBDocument")
}

func (r *insertOnlyStore) RestoreKBDocument(context.Context, repository.KBScope, string, string) (*repository.KBDocument, error) {
	return nil, r.wrote("RestoreKBDocument")
}

// reconciler returns a service on the test Typesense whose store is repo
// behind insertOnlyStore.
func reconciler(t *testing.T, repo *repository.Repo) *Service {
	t.Helper()
	r, _ := reconcilerStore(t, repo)
	return r
}

// reconcilerStore is reconciler, also returning its store to count inserts.
func reconcilerStore(t *testing.T, repo *repository.Repo) (*Service, *insertOnlyStore) {
	t.Helper()
	url, key := typesenseEnv(t)
	r := New(url, key, "", "")
	st := &insertOnlyStore{Store: repo, t: t}
	r.SetStore(st)
	return r, st
}

// backfilledKB is storedService with the KB's backfill marked complete.
func backfilledKB(t *testing.T) (*Service, *repository.Repo, Scope) {
	t.Helper()
	s, repo, sc := storedService(t)
	if err := repo.CompleteKBBackfill(context.Background(), storeScope(sc), 0); err != nil {
		t.Fatal(err)
	}
	return s, repo, sc
}

// rowsSnapshot is every row of sc (live and deleted) as id|version|updated|deleted.
func rowsSnapshot(t *testing.T, sc Scope) []string {
	t.Helper()
	rows, err := rawDB(t).Query(context.Background(), `SELECT id, version, updated_at, deleted_at IS NOT NULL,
		annotations::text FROM kb_documents WHERE scope_kind = $1 AND scope_id = $2 ORDER BY id`, sc.Kind, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		var v int
		var u any
		var del bool
		var anns string
		if err := rows.Scan(&id, &v, &u, &del, &anns); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d|%d|%v|%v|%s", id, v, u, del, anns))
	}
	return out
}

func mustReconcile(t *testing.T, r *Service, sc Scope) ReconcileStats {
	t.Helper()
	st, err := r.ReconcileKB(context.Background(), sc)
	if err != nil {
		t.Fatalf("ReconcileKB: %v", err)
	}
	return st
}

// A doc written while Typesense was down is saved in Postgres only; the
// next reconcile indexes it, and Postgres is left exactly as it was.
func TestReconcileIndexesDocWrittenWhileTypesenseDown(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "up.md", "Up", "written while typesense was up", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	_, key := typesenseEnv(t)
	down := New("http://127.0.0.1:1", key, "", "")
	down.SetStore(repo)
	if _, err := down.WriteDoc(ctx, sc, "down.md", "Outage note", "zebracorn written while typesense was down", "me", []string{"ops"}, "", nil); err != nil {
		t.Fatalf("write with typesense down: %v", err)
	}
	if got := searchPaths(t, s, sc, "zebracorn"); len(got) != 0 {
		t.Fatalf("doc searchable before reconcile: %v", got)
	}
	before := rowsSnapshot(t, sc)

	r := reconciler(t, repo)
	st := mustReconcile(t, r, sc)
	if st.Live != 2 || st.Missing != 1 || st.Stale != 0 || st.Removed != 0 || st.Failed != 0 || st.Skipped != "" {
		t.Fatalf("stats = %+v, want 2 live, 1 missing", st)
	}
	if got := searchPaths(t, s, sc, "zebracorn"); !contains(got, "down.md") {
		t.Fatalf("doc not searchable after reconcile: %v", got)
	}
	d, err := s.getIndexed(ctx, sc, "down.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := mustRow(t, repo, sc, "down.md").UpdatedAt.Unix(); d.UpdatedAt != want {
		t.Errorf("indexed updated_at = %d, want the row's %d", d.UpdatedAt, want)
	}
	if after := rowsSnapshot(t, sc); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("postgres changed by reconcile:\nbefore %v\nafter  %v", before, after)
	}

	// A second pass finds nothing to do.
	if st := mustReconcile(t, r, sc); st.Fixed() != 0 || st.Indexed != 2 {
		t.Errorf("second pass stats = %+v, want nothing fixed", st)
	}
}

// Rows changed behind the index's back (title, a content-only edit within
// the same second, annotations) are re-indexed from the row.
func TestReconcileUpdatesStaleEntries(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	rs := storeScope(sc)
	for _, p := range []string{"title.md", "content.md", "notes.md", "same.md"} {
		if _, err := s.WriteDoc(ctx, sc, p, p, "first "+p, "me", nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	// Change rows directly in Postgres, without indexing.
	if _, _, err := repo.UpsertKBDocument(ctx, rs, repository.KBDocWrite{Path: "title.md", Title: "Renamed", Content: "first title.md", Author: "me", By: "me"}); err != nil {
		t.Fatal(err)
	}
	// Same title and metadata: only the content (and version) change, and
	// updated_at may well land in the same second.
	if _, _, err := repo.UpsertKBDocument(ctx, rs, repository.KBDocWrite{Path: "content.md", Title: "content.md", Content: "second quokkafish", Author: "me", By: "me"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateKBAnnotations(ctx, rs, "notes.md", func(a []domain.KBAnnotation) ([]domain.KBAnnotation, error) {
		return append(a, domain.KBAnnotation{Author: "me", Text: "a note", CreatedAt: 1}), nil
	}); err != nil {
		t.Fatal(err)
	}

	st := mustReconcile(t, reconciler(t, repo), sc)
	if st.Stale != 3 || st.Missing != 0 || st.Failed != 0 {
		t.Fatalf("stats = %+v, want 3 stale", st)
	}
	if d, _ := s.getIndexed(ctx, sc, "title.md"); d == nil || d.Title != "Renamed" {
		t.Errorf("title.md indexed as %+v, want title Renamed", d)
	}
	if d, _ := s.getIndexed(ctx, sc, "content.md"); d == nil || d.Content != "second quokkafish" {
		t.Errorf("content.md indexed as %+v, want the new content", d)
	}
	if d, _ := s.getIndexed(ctx, sc, "notes.md"); d == nil || len(d.Annotations) != 1 {
		t.Errorf("notes.md indexed as %+v, want 1 annotation", d)
	}
}

// An index entry whose row is soft-deleted is removed; an entry with no row
// at all is left alone and only counted.
func TestReconcileRemovesDeletedDocs(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	for _, p := range []string{"keep.md", "gone.md"} {
		if _, err := s.WriteDoc(ctx, sc, p, p, "content of "+p, "me", nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.DeleteKBDocument(ctx, storeScope(sc), "gone.md", "me"); err != nil {
		t.Fatal(err)
	}
	// updated_at 0: older than the backfill, so it is not adopted.
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "orphan.md", title: "orphan", content: "no row"}); err != nil {
		t.Fatal(err)
	}
	if chunkCount(t, s, sc, "gone.md") == 0 {
		t.Fatal("setup: gone.md not in the index")
	}

	st := mustReconcile(t, reconciler(t, repo), sc)
	if st.Removed != 1 || st.IndexOnly != 1 || st.Adopted != 0 || st.Missing != 0 || st.Stale != 0 || st.Failed != 0 {
		t.Fatalf("stats = %+v, want 1 removed, 1 index-only", st)
	}
	if n := chunkCount(t, s, sc, "gone.md"); n != 0 {
		t.Errorf("deleted doc still has %d index entries", n)
	}
	if n := chunkCount(t, s, sc, "keep.md"); n == 0 {
		t.Error("live doc removed from the index")
	}
	if n := chunkCount(t, s, sc, "orphan.md"); n == 0 {
		t.Error("index entry with no postgres row was removed")
	}
	if deleted, exists := deletedRow(t, rawDB(t), sc, "gone.md"); !deleted || !exists {
		t.Errorf("gone.md row: deleted=%v exists=%v, want still soft-deleted", deleted, exists)
	}
}

// A KB whose backfill hasn't completed is never touched: Postgres may not
// hold every doc yet.
func TestReconcileSkipsKBNotBackfilled(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	if _, _, err := repo.UpsertKBDocument(ctx, storeScope(sc), repository.KBDocWrite{Path: "a.md", Title: "a", Content: "a", Author: "me", By: "me"}); err != nil {
		t.Fatal(err)
	}
	st := mustReconcile(t, reconciler(t, repo), sc)
	if st.Skipped != "backfill not complete" || st.Fixed() != 0 {
		t.Fatalf("stats = %+v, want skipped for the backfill", st)
	}
	if kc := kbCols(t, s, sc); kc.real || kc.alias != "" {
		t.Errorf("reconcile created an index for a KB it should skip: %+v", kc)
	}
}

// The reconciler doesn't run on a KB whose reindex lock is held (a reindex
// or Drop), and a pass doesn't run while another replica holds the
// reconcile lock.
func TestReconcileRespectsLocks(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	if _, _, err := repo.UpsertKBDocument(ctx, storeScope(sc), repository.KBDocWrite{Path: "a.md", Title: "a", Content: "a", Author: "me", By: "me"}); err != nil {
		t.Fatal(err)
	}
	r := reconciler(t, repo)
	r.SetReindexLock(repo.TryNamedLock)
	r.reconcileOnly = func(x Scope) bool { return x == sc }

	// Another replica's reindex holds this KB's lock.
	other := reconciler(t, repo)
	other.SetReindexLock(repo.TryNamedLock)
	release, err := other.beginReindex(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if st := mustReconcile(t, r, sc); st.Skipped == "" || st.Fixed() != 0 {
		t.Fatalf("reconciled during a reindex: %+v", st)
	}
	release()

	if err := s.ensureCollection(ctx, sc); err != nil { // so chunkCount can count
		t.Fatal(err)
	}

	// Another replica is reconciling: this one's pass does nothing.
	unlock, ok, err := repo.TryNamedLock(ctx, reconcileLockKey)
	if err != nil || !ok {
		t.Fatalf("take the reconcile lock: ok=%v err=%v", ok, err)
	}
	r.reconcilePass(ctx, repo.TryNamedLock)
	if n := chunkCount(t, s, sc, "a.md"); n != 0 {
		t.Fatalf("a pass ran while another replica held the reconcile lock (%d entries)", n)
	}
	unlock()
	r.reconcilePass(ctx, repo.TryNamedLock)
	if n := chunkCount(t, s, sc, "a.md"); n == 0 {
		t.Fatal("pass with the lock free did not index the doc")
	}
}

// More docs than one index page (250) are all compared and indexed.
func TestReconcilePaginates(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	const n = 260
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("d%03d.md", i)
		if _, _, err := repo.UpsertKBDocument(ctx, storeScope(sc), repository.KBDocWrite{Path: p, Title: p, Content: "c", Author: "me", By: "me"}); err != nil {
			t.Fatal(err)
		}
	}
	r := reconciler(t, repo)
	if st := mustReconcile(t, r, sc); st.Missing != n || st.Failed != 0 {
		t.Fatalf("first pass = %+v, want %d missing", st, n)
	}
	if got := mustCount(t, s, sc); got != n {
		t.Fatalf("index holds %d docs, want %d", got, n)
	}
	if st := mustReconcile(t, r, sc); st.Indexed != n || st.Fixed() != 0 {
		t.Fatalf("second pass = %+v, want %d indexed and nothing fixed", st, n)
	}
}

// An entry with the row's metadata but another version is stale (a content
// edit within the same second); an entry without a version (written before
// kb_version existed) is judged on its metadata alone.
func TestIndexStateCurrent(t *testing.T) {
	row := indexState{key: "k", version: 3}
	for _, tc := range []struct {
		name string
		ix   indexState
		want bool
	}{
		{"same", indexState{key: "k", version: 3}, true},
		{"older version", indexState{key: "k", version: 2}, false},
		{"no version", indexState{key: "k"}, true},
		{"metadata differs", indexState{key: "other", version: 3}, false},
		{"no version, metadata differs", indexState{key: "other"}, false},
	} {
		if got := tc.ix.current(row); got != tc.want {
			t.Errorf("%s: current = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// oldPodWrite indexes d straight into Typesense the way a pre-Postgres pod
// does during a rolling deploy: no Postgres row, and no kb_version.
func oldPodWrite(t *testing.T, s *Service, sc Scope, d indexedDoc) {
	t.Helper()
	d.version = 0
	if _, err := s.indexChunks(context.Background(), sc, d); err != nil {
		t.Fatal(err)
	}
}

// indexedVersion is the kb_version on path's chunk 0 (0 when absent).
func indexedVersion(t *testing.T, s *Service, sc Scope, path string) int {
	t.Helper()
	m, err := s.client.Collection(collectionName(sc)).Document(docID(sc, path, 0)).Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	v, _ := m[indexedVersionField].(float64)
	return int(v)
}

// A doc an old pod wrote straight to Typesense after the KB's backfill (the
// rolling-deploy gap of #1994) is copied into Postgres by one reconcile,
// with its original updated_at, content and annotations, and re-indexed
// from the new row. Nothing else in Postgres changes.
func TestReconcileAdoptsIndexOnlyDoc(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "keep.md", "Keep", "a stored doc", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	before := rowsSnapshot(t, sc)

	updated := time.Now().Add(-30 * time.Second).Unix()
	anns := []Annotation{
		{Author: "ann@x", Text: "first note", CreatedAt: updated - 10},
		{Author: "bob@x", Text: "resolved note", CreatedAt: updated - 5, IsResolved: true},
	}
	// Long enough for several chunks: the copy must take chunk 0's full content.
	content := "# Old pod\n\n" + strings.Repeat("wombatfeather paragraph text for chunking. ", 400)
	oldPodWrite(t, s, sc, indexedDoc{
		path: "oldpod.md", title: "Old pod doc", content: content, author: "old@x", reason: "written mid-deploy",
		tags: []string{"deploy", "kb"}, annotations: anns, updatedAt: updated,
	})
	if chunkCount(t, s, sc, "oldpod.md") < 2 {
		t.Fatal("setup: want a multi-chunk doc")
	}

	r, store := reconcilerStore(t, repo)
	st := mustReconcile(t, r, sc)
	if st.Adopted != 1 || st.IndexOnly != 0 || st.Failed != 0 || st.Fixed() != 0 {
		t.Fatalf("stats = %+v, want 1 adopted and nothing else", st)
	}
	if n := store.inserts.Load(); n != 1 {
		t.Errorf("inserts = %d, want 1", n)
	}

	row := mustRow(t, repo, sc, "oldpod.md")
	if row.UpdatedAt.Unix() != updated || row.CreatedAt.Unix() != updated {
		t.Errorf("row dates = created %v updated %v, want both %d", row.CreatedAt.Unix(), row.UpdatedAt.Unix(), updated)
	}
	if row.Content != content || row.Title != "Old pod doc" || row.Author != "old@x" || row.Reason != "written mid-deploy" {
		t.Errorf("row = %q/%q/%q, content match %v", row.Title, row.Author, row.Reason, row.Content == content)
	}
	if !reflect.DeepEqual(row.Tags, []string{"deploy", "kb"}) {
		t.Errorf("row tags = %v", row.Tags)
	}
	if !reflect.DeepEqual(row.Annotations, anns) {
		t.Errorf("row annotations = %+v, want %+v", row.Annotations, anns)
	}
	if row.Version != 1 {
		t.Errorf("row version = %d, want 1", row.Version)
	}
	var by, change string
	if err := rawDB(t).QueryRow(ctx, `SELECT v.changed_by, v.change FROM kb_document_versions v
		JOIN kb_documents d ON d.id = v.document_id
		WHERE d.scope_kind = $1 AND d.scope_id = $2 AND d.path = 'oldpod.md'`, sc.Kind, sc.ID).Scan(&by, &change); err != nil {
		t.Fatal(err)
	}
	if by != reconcileAdoptBy || change != "backfill" {
		t.Errorf("version row = %s/%s, want %s/backfill", by, change, reconcileAdoptBy)
	}

	// Re-indexed from the row: the entry now carries the row's version and
	// the doc is still searchable with its original date.
	if v := indexedVersion(t, s, sc, "oldpod.md"); v != 1 {
		t.Errorf("indexed kb_version = %d, want 1", v)
	}
	if d, err := s.getIndexed(ctx, sc, "oldpod.md"); err != nil || d.UpdatedAt != updated || len(d.Annotations) != 2 {
		t.Errorf("indexed copy = %+v (err %v), want updated_at %d and 2 annotations", d, err, updated)
	}
	if got := searchPaths(t, s, sc, "wombatfeather"); !contains(got, "oldpod.md") {
		t.Errorf("adopted doc not searchable: %v", got)
	}

	// The other row is untouched; only the adopted row was added.
	after := rowsSnapshot(t, sc)
	if len(after) != len(before)+1 || fmt.Sprint(after[:len(before)]) != fmt.Sprint(before) {
		t.Errorf("postgres rows changed:\nbefore %v\nafter  %v", before, after)
	}

	// A second pass finds nothing to do.
	if st := mustReconcile(t, r, sc); st.Adopted != 0 || st.Fixed() != 0 || st.IndexOnly != 0 || st.Live != 2 {
		t.Errorf("second pass = %+v, want nothing to do", st)
	}
	if n := store.inserts.Load(); n != 1 {
		t.Errorf("inserts after the second pass = %d, want still 1", n)
	}
}

// An old pod's write to a path whose row is soft-deleted does not revive
// it: the entry is removed as before and the row stays deleted, unchanged.
func TestReconcileDoesNotReviveDeletedPath(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "gone.md", "Gone", "deleted doc", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDoc(ctx, sc, "gone.md", "me"); err != nil {
		t.Fatal(err)
	}
	oldPodWrite(t, s, sc, indexedDoc{path: "gone.md", title: "Gone again", content: "old pod rewrite",
		author: "old@x", updatedAt: time.Now().Unix()})
	before := rowsSnapshot(t, sc)

	r, store := reconcilerStore(t, repo)
	st := mustReconcile(t, r, sc)
	if st.Removed != 1 || st.Adopted != 0 || st.IndexOnly != 0 || st.Failed != 0 {
		t.Fatalf("stats = %+v, want 1 removed and nothing adopted", st)
	}
	if n := store.inserts.Load(); n != 0 {
		t.Errorf("inserts = %d, want 0", n)
	}
	if n := chunkCount(t, s, sc, "gone.md"); n != 0 {
		t.Errorf("deleted path still has %d index entries", n)
	}
	if after := rowsSnapshot(t, sc); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("postgres changed:\nbefore %v\nafter  %v", before, after)
	}

	// copyIndexed itself refuses the path too (a row appeared between the
	// reconciler's listing and its copy).
	oldPodWrite(t, s, sc, indexedDoc{path: "gone.md", title: "Gone again", content: "x", updatedAt: time.Now().Unix()})
	inserted, found, err := r.copyIndexed(ctx, sc, "gone.md", reconcileAdoptBy)
	if err != nil || !found || inserted {
		t.Errorf("copyIndexed over a deleted row = inserted %v found %v err %v, want found and not inserted", inserted, found, err)
	}
	if deleted, exists := deletedRow(t, rawDB(t), sc, "gone.md"); !deleted || !exists {
		t.Errorf("gone.md row: deleted=%v exists=%v, want still soft-deleted", deleted, exists)
	}
}

// Index-only entries that are not an old pod's post-backfill write stay in
// place: one with a kb_version (it was indexed from a row that is gone now)
// and one dated before the backfill started.
func TestReconcileLeavesOtherIndexOnlyEntries(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	now := time.Now().Unix()
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "purged.md", title: "p", content: "had a row", updatedAt: now, version: 3}); err != nil {
		t.Fatal(err)
	}
	oldPodWrite(t, s, sc, indexedDoc{path: "ancient.md", title: "a", content: "before the backfill", updatedAt: now - 86400})

	r, store := reconcilerStore(t, repo)
	st := mustReconcile(t, r, sc)
	if st.IndexOnly != 2 || st.Adopted != 0 || st.Failed != 0 {
		t.Fatalf("stats = %+v, want 2 index-only, none adopted", st)
	}
	if n := store.inserts.Load(); n != 0 {
		t.Errorf("inserts = %d, want 0", n)
	}
	for _, p := range []string{"purged.md", "ancient.md"} {
		if chunkCount(t, s, sc, p) == 0 {
			t.Errorf("%s removed from the index", p)
		}
		if _, exists := deletedRow(t, rawDB(t), sc, p); exists {
			t.Errorf("%s got a postgres row", p)
		}
	}
}

func TestAdoptable(t *testing.T) {
	started := time.Unix(1_000_000, 0)
	slack := int64(adoptClockSlack / time.Second)
	for _, tc := range []struct {
		name string
		ix   indexState
		want bool
	}{
		{"after the backfill, no version", indexState{updatedAt: 1_000_100}, true},
		{"within the clock slack", indexState{updatedAt: 1_000_000 - slack}, true},
		{"before the slack", indexState{updatedAt: 1_000_000 - slack - 1}, false},
		{"has a version", indexState{updatedAt: 1_000_100, version: 2}, false},
		{"no updated_at", indexState{}, false},
	} {
		if got := adoptable(tc.ix, started); got != tc.want {
			t.Errorf("%s: adoptable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// versionRows is path's kb_document_versions as version|change|changed_by|content.
func versionRows(t *testing.T, sc Scope, path string) []string {
	t.Helper()
	rows, err := rawDB(t).Query(context.Background(), `SELECT v.version, v.change, v.changed_by, v.content
		FROM kb_document_versions v JOIN kb_documents d ON d.id = v.document_id
		WHERE d.scope_kind = $1 AND d.scope_id = $2 AND d.path = $3 ORDER BY v.version`, sc.Kind, sc.ID, path)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v int
		var change, by, content string
		if err := rows.Scan(&v, &change, &by, &content); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d|%s|%s|%s", v, change, by, content))
	}
	return out
}

// annotatedDoc writes path through s with one annotation, resolved, and
// returns its row.
func annotatedDoc(t *testing.T, s *Service, repo *repository.Repo, sc Scope, path, content string) *repository.KBDocument {
	t.Helper()
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, path, "Doc", content, "me", []string{"a"}, "why", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnnotateDoc(ctx, sc, path, "first note", "ann@x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAnnotation(ctx, sc, path, 0, true, "res@x"); err != nil {
		t.Fatal(err)
	}
	return mustRow(t, repo, sc, path)
}

// An old pod (rolling deploy, or the old image after a rollback) edits a
// doc that has a live row and adds an annotation: its entry has new content,
// a newer updated_at and no kb_version. The reconciler copies the edit into
// the row (the previous content stays in kb_document_versions), merges the
// annotations, and re-indexes from the row instead of overwriting the edit.
func TestReconcileAdoptsOldPodEdit(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	row := annotatedDoc(t, s, repo, sc, "doc.md", "original content")
	if row.Version != 1 || len(row.Annotations) != 1 || !row.Annotations[0].IsResolved {
		t.Fatalf("setup row = v%d %+v", row.Version, row.Annotations)
	}
	other := annotatedDoc(t, s, repo, sc, "other.md", "untouched")

	// The old pod read the doc from Typesense (it saw the note unresolved
	// if its read predates the resolve), added a note and rewrote it.
	seen := row.Annotations[0]
	seen.IsResolved, seen.ResolvedBy, seen.ResolvedAt = false, "", 0
	added := Annotation{Author: "old@x", Text: "note from the old pod", CreatedAt: row.UpdatedAt.Unix() + 1}
	newer := row.UpdatedAt.Unix() + 2
	oldPodWrite(t, s, sc, indexedDoc{path: "doc.md", title: "Doc v2", content: "koalaquill edited by an old pod",
		author: "old@x", reason: "why", tags: []string{"a", "b"}, annotations: []Annotation{seen, added}, updatedAt: newer})

	r, store := reconcilerStore(t, repo)
	store.allowEdits = true
	st := mustReconcile(t, r, sc)
	if st.AdoptedEdits != 1 || st.Stale != 0 || st.Missing != 0 || st.Adopted != 0 || st.Failed != 0 {
		t.Fatalf("stats = %+v, want 1 adopted edit and nothing else", st)
	}
	if u, a := store.upserts.Load(), store.annUpdates.Load(); u != 1 || a != 1 {
		t.Errorf("upserts = %d, annotation updates = %d, want 1 and 1", u, a)
	}

	got := mustRow(t, repo, sc, "doc.md")
	if got.Content != "koalaquill edited by an old pod" || got.Title != "Doc v2" || got.Author != "old@x" ||
		!reflect.DeepEqual(got.Tags, []string{"a", "b"}) || got.Version != 2 {
		t.Errorf("row = v%d %q %q %q %v, want the old pod's edit at v2", got.Version, got.Title, got.Author, got.Content, got.Tags)
	}
	wantAnns := []Annotation{row.Annotations[0], added} // the row's resolved state kept
	if !reflect.DeepEqual(got.Annotations, wantAnns) {
		t.Errorf("row annotations = %+v, want %+v", got.Annotations, wantAnns)
	}
	vers := versionRows(t, sc, "doc.md")
	if len(vers) != 2 || vers[0] != "1|create|me|original content" ||
		vers[1] != "2|update|"+reconcileEditBy+"|koalaquill edited by an old pod" {
		t.Errorf("versions = %q, want the original at v1 and the edit at v2 by %s", vers, reconcileEditBy)
	}
	if o := mustRow(t, repo, sc, "other.md"); !reflect.DeepEqual(o, other) {
		t.Errorf("other row changed: %+v", o)
	}

	// The entry is re-indexed from the row: kb_version, the new content,
	// both annotations.
	if v := indexedVersion(t, s, sc, "doc.md"); v != 2 {
		t.Errorf("indexed kb_version = %d, want 2", v)
	}
	d, err := s.getIndexed(ctx, sc, "doc.md")
	if err != nil || d.Content != got.Content || !reflect.DeepEqual(d.Annotations, wantAnns) || d.UpdatedAt != got.UpdatedAt.Unix() {
		t.Errorf("indexed copy = %+v (err %v)", d, err)
	}

	// A second pass is a no-op.
	before := rowsSnapshot(t, sc)
	if st := mustReconcile(t, r, sc); st.AdoptedEdits != 0 || st.Fixed() != 0 || st.Failed != 0 {
		t.Errorf("second pass = %+v, want nothing to do", st)
	}
	if u, a := store.upserts.Load(), store.annUpdates.Load(); u != 1 || a != 1 {
		t.Errorf("after the second pass upserts = %d, annotation updates = %d, want still 1 and 1", u, a)
	}
	if after := rowsSnapshot(t, sc); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("second pass changed postgres:\nbefore %v\nafter  %v", before, after)
	}
}

// Old code's annotate keeps the doc's updated_at, so an old pod's new
// annotation leaves an entry dated the same second as its row. The note is
// merged into the row; the content and version are left alone.
func TestReconcileAdoptsOldPodAnnotation(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	row := annotatedDoc(t, s, repo, sc, "doc.md", "content")
	added := Annotation{Author: "old@x", Text: "annotated on the old image", CreatedAt: row.UpdatedAt.Unix() + 60}
	oldPodWrite(t, s, sc, indexedDoc{path: "doc.md", title: row.Title, content: row.Content, author: row.Author,
		reason: row.Reason, tags: row.Tags, annotations: append(slices.Clone(row.Annotations), added), updatedAt: row.UpdatedAt.Unix()})

	r, store := reconcilerStore(t, repo)
	store.allowEdits = true
	st := mustReconcile(t, r, sc)
	if st.AdoptedEdits != 1 || st.Stale != 0 || st.Failed != 0 {
		t.Fatalf("stats = %+v, want 1 adopted edit", st)
	}
	if u, a := store.upserts.Load(), store.annUpdates.Load(); u != 0 || a != 1 {
		t.Errorf("upserts = %d, annotation updates = %d, want 0 and 1", u, a)
	}
	got := mustRow(t, repo, sc, "doc.md")
	if want := append(slices.Clone(row.Annotations), added); !reflect.DeepEqual(got.Annotations, want) {
		t.Errorf("row annotations = %+v, want %+v", got.Annotations, want)
	}
	if got.Version != row.Version || !got.UpdatedAt.Equal(row.UpdatedAt) || got.Content != row.Content {
		t.Errorf("row changed beyond its annotations: v%d %v", got.Version, got.UpdatedAt)
	}
	if v := indexedVersion(t, s, sc, "doc.md"); v != row.Version {
		t.Errorf("indexed kb_version = %d, want %d", v, row.Version)
	}
	if st := mustReconcile(t, r, sc); st.AdoptedEdits != 0 || st.Fixed() != 0 {
		t.Errorf("second pass = %+v, want nothing to do", st)
	}
	if a := store.annUpdates.Load(); a != 1 {
		t.Errorf("annotation updates after the second pass = %d, want 1", a)
	}
}

// Stale entries that are not an old pod's later edit are re-indexed from
// the row and Postgres is not written (allowEdits is off, so any write fails
// the test): one with no kb_version dated before its row (an index job
// failed after a new-pod write), and one with a kb_version.
func TestReconcileStaleEntriesNotOldPodEdits(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	older := annotatedDoc(t, s, repo, sc, "older.md", "row content")
	versioned := annotatedDoc(t, s, repo, sc, "versioned.md", "row content")
	extra := Annotation{Author: "x@x", Text: "only in the entry", CreatedAt: 5}
	oldPodWrite(t, s, sc, indexedDoc{path: "older.md", title: "Old title", content: "older entry content",
		annotations: []Annotation{extra}, updatedAt: older.UpdatedAt.Unix() - 100})
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "versioned.md", title: "Other", content: "versioned entry content",
		annotations: []Annotation{extra}, updatedAt: versioned.UpdatedAt.Unix() + 100, version: versioned.Version}); err != nil {
		t.Fatal(err)
	}
	before := rowsSnapshot(t, sc)

	r, store := reconcilerStore(t, repo)
	st := mustReconcile(t, r, sc)
	if st.Stale != 2 || st.AdoptedEdits != 0 || st.Failed != 0 {
		t.Fatalf("stats = %+v, want 2 stale, no adopted edits", st)
	}
	if n := store.writes.Load(); n != 0 {
		t.Errorf("%d refused writes", n)
	}
	if after := rowsSnapshot(t, sc); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("postgres changed:\nbefore %v\nafter  %v", before, after)
	}
	for _, row := range []*repository.KBDocument{older, versioned} {
		d, err := s.getIndexed(ctx, sc, row.Path)
		if err != nil || d.Content != row.Content || d.Title != row.Title || !reflect.DeepEqual(d.Annotations, row.Annotations) {
			t.Errorf("%s indexed as %+v (err %v), want the row", row.Path, d, err)
		}
		if v := indexedVersion(t, s, sc, row.Path); v != row.Version {
			t.Errorf("%s kb_version = %d, want %d", row.Path, v, row.Version)
		}
	}
	if st := mustReconcile(t, r, sc); st.Fixed() != 0 || st.AdoptedEdits != 0 {
		t.Errorf("second pass = %+v, want nothing to do", st)
	}
}

func TestOldPodEdit(t *testing.T) {
	row := indexState{key: "k", version: 2, updatedAt: 100}
	for _, tc := range []struct {
		name string
		ix   indexState
		want bool
	}{
		{"newer, no version", indexState{updatedAt: 101}, true},
		{"same second, no version", indexState{updatedAt: 100}, true},
		{"older, no version", indexState{updatedAt: 99}, false},
		{"newer, has a version", indexState{updatedAt: 101, version: 2}, false},
	} {
		if got := oldPodEdit(tc.ix, row); got != tc.want {
			t.Errorf("%s: oldPodEdit = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMergeAnnotations(t *testing.T) {
	a := Annotation{Author: "a", Text: "one", CreatedAt: 10, IsResolved: true, ResolvedBy: "r", ResolvedAt: 20}
	aOpen := Annotation{Author: "a", Text: "one", CreatedAt: 10}
	b := Annotation{Author: "b", Text: "two", CreatedAt: 5}
	c := Annotation{Author: "c", Text: "three", CreatedAt: 30}
	for _, tc := range []struct {
		name       string
		row, entry []Annotation
		want       []Annotation
	}{
		{"row's resolved state wins", []Annotation{a}, []Annotation{aOpen}, []Annotation{a}},
		{"union, oldest first", []Annotation{a, c}, []Annotation{aOpen, b}, []Annotation{b, a, c}},
		{"nothing in the entry keeps the row", []Annotation{a}, nil, []Annotation{a}},
		{"empty row", nil, []Annotation{c}, []Annotation{c}},
		{"both empty", nil, nil, []Annotation{}},
	} {
		if got := mergeAnnotations(tc.row, tc.entry); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
