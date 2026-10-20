package kb

// Tests for the reconciler (#1993). They need Postgres and Typesense; see
// store_test.go for the environment. Every reconcile here runs on a
// service whose store refuses writes, so each test also proves the
// reconciler never modifies Postgres.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// readOnlyStore is a Store that fails the test on any write.
type readOnlyStore struct {
	Store
	t      *testing.T
	writes atomic.Int32
}

func (r *readOnlyStore) wrote(name string) error {
	r.writes.Add(1)
	r.t.Errorf("reconciler wrote postgres: %s", name)
	return fmt.Errorf("read-only store: %s refused", name)
}

func (r *readOnlyStore) UpsertKBDocument(context.Context, repository.KBScope, repository.KBDocWrite) (*repository.KBDocument, bool, error) {
	return nil, false, r.wrote("UpsertKBDocument")
}

func (r *readOnlyStore) UpdateKBAnnotations(context.Context, repository.KBScope, string,
	func([]domain.KBAnnotation) ([]domain.KBAnnotation, error)) (*repository.KBDocument, error) {
	return nil, r.wrote("UpdateKBAnnotations")
}

func (r *readOnlyStore) InsertKBDocumentIfMissing(context.Context, repository.KBScope, repository.KBDocument, string) (bool, error) {
	return false, r.wrote("InsertKBDocumentIfMissing")
}

func (r *readOnlyStore) DeleteKBDocument(context.Context, repository.KBScope, string, string) error {
	return r.wrote("DeleteKBDocument")
}

func (r *readOnlyStore) RestoreKBDocument(context.Context, repository.KBScope, string, string) (*repository.KBDocument, error) {
	return nil, r.wrote("RestoreKBDocument")
}

// reconciler returns a service on the test Typesense whose store is repo
// behind readOnlyStore.
func reconciler(t *testing.T, repo *repository.Repo) *Service {
	t.Helper()
	url, key := typesenseEnv(t)
	r := New(url, key, "", "")
	r.SetStore(&readOnlyStore{Store: repo, t: t})
	return r
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
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "orphan.md", title: "orphan", content: "no row"}); err != nil {
		t.Fatal(err)
	}
	if chunkCount(t, s, sc, "gone.md") == 0 {
		t.Fatal("setup: gone.md not in the index")
	}

	st := mustReconcile(t, reconciler(t, repo), sc)
	if st.Removed != 1 || st.IndexOnly != 1 || st.Missing != 0 || st.Stale != 0 || st.Failed != 0 {
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
