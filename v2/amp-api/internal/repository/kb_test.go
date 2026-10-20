package repository

// Tests for kb.go against a real Postgres. They run only when
// TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository/... -race -count=1
//
// The migrations are read from cmd/server/migrations (they're embedded in
// package main, which this package can't import). Each test uses its own
// random scope ids, so tests and repeated runs don't see each other's rows.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/domain"
)

func kbTestRepo(t *testing.T) *Repo {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the KB repository tests against Postgres")
	}
	ctx := context.Background()
	r, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(r.Close)

	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "server", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	var ms []Migration
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		ms = append(ms, Migration{Version: strings.TrimSuffix(filepath.Base(f), ".sql"), SQL: string(b)})
	}
	if err := r.RunMigrations(ctx, ms); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return r
}

// newScope returns a scope of the given kind with an id no other test uses.
func newScope(kind string) KBScope {
	return KBScope{Kind: kind, ID: 1_000_000 + rand.IntN(1_000_000_000)}
}

// kbVersions returns a doc's version rows as "version:change", in order.
func kbVersions(t *testing.T, r *Repo, docID int64) []string {
	t.Helper()
	rows, err := r.db.Query(context.Background(),
		`SELECT version, change FROM kb_document_versions WHERE document_id = $1 ORDER BY version`, docID)
	if err != nil {
		t.Fatalf("query versions: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v int
		var c string
		if err := rows.Scan(&v, &c); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		out = append(out, fmt.Sprintf("%d:%s", v, c))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("versions: %v", err)
	}
	return out
}

func mustUpsert(t *testing.T, r *Repo, scope KBScope, w KBDocWrite) (*KBDocument, bool) {
	t.Helper()
	d, changed, err := r.UpsertKBDocument(context.Background(), scope, w)
	if err != nil {
		t.Fatalf("upsert %s: %v", w.Path, err)
	}
	return d, changed
}

func TestKBRejectsUnstoredScopes(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()
	for _, kind := range []string{"global", "", "Project"} {
		t.Run(kind, func(t *testing.T) {
			s := KBScope{Kind: kind, ID: 1}
			if _, _, err := r.UpsertKBDocument(ctx, s, KBDocWrite{Path: "a.md"}); err == nil {
				t.Error("UpsertKBDocument accepted the scope")
			}
			if _, err := r.GetKBDocument(ctx, s, "a.md"); err == nil {
				t.Error("GetKBDocument accepted the scope")
			}
			if _, err := r.InsertKBDocumentIfMissing(ctx, s, KBDocument{Path: "a.md"}, "x"); err == nil {
				t.Error("InsertKBDocumentIfMissing accepted the scope")
			}
			if err := r.StartKBBackfill(ctx, s); err == nil {
				t.Error("StartKBBackfill accepted the scope")
			}
		})
	}
}

func TestKBUpsertVersions(t *testing.T) {
	r := kbTestRepo(t)
	scope := newScope(KBScopeProject)
	base := KBDocWrite{Path: "arch/overview.md", Title: "Overview", Tags: []string{"a", "b"},
		Author: "alice", Reason: "", Content: "v1", By: "alice"}

	created, changed := mustUpsert(t, r, scope, base)
	if !changed || created.Version != 1 || created.Content != "v1" {
		t.Fatalf("create: changed=%v version=%d content=%q", changed, created.Version, created.Content)
	}

	steps := []struct {
		name        string
		edit        func(w *KBDocWrite)
		wantChanged bool
		wantVersion int
		wantTouched bool // updated_at moves forward; otherwise it must stay put
	}{
		{"identical write is a no-op", func(w *KBDocWrite) {}, false, 1, false},
		{"author only: row written, no version", func(w *KBDocWrite) { w.Author = "bob" }, true, 1, false},
		{"reason only: row written, no version", func(w *KBDocWrite) { w.Reason = "shared" }, true, 1, false},
		{"content change", func(w *KBDocWrite) { w.Content = "v2" }, true, 2, true},
		{"title change", func(w *KBDocWrite) { w.Title = "Overview 2" }, true, 3, true},
		{"tags change", func(w *KBDocWrite) { w.Tags = []string{"a", "c"} }, true, 4, true},
		{"tag order counts as a change", func(w *KBDocWrite) { w.Tags = []string{"c", "a"} }, true, 5, true},
		{"nil tags vs empty tags", func(w *KBDocWrite) { w.Tags = nil }, true, 6, true},
		{"empty tags again is a no-op", func(w *KBDocWrite) { w.Tags = []string{} }, false, 6, false},
	}
	w := base
	prev := created.UpdatedAt
	for _, st := range steps {
		st.edit(&w)
		time.Sleep(2 * time.Millisecond) // NOW() must be able to differ from prev
		d, changed := mustUpsert(t, r, scope, w)
		if changed != st.wantChanged || d.Version != st.wantVersion {
			t.Errorf("%s: changed=%v version=%d, want changed=%v version=%d",
				st.name, changed, d.Version, st.wantChanged, st.wantVersion)
		}
		if st.wantTouched && !d.UpdatedAt.After(prev) {
			t.Errorf("%s: updated_at = %v, want after %v", st.name, d.UpdatedAt, prev)
		}
		if !st.wantTouched && !d.UpdatedAt.Equal(prev) {
			t.Errorf("%s: updated_at moved %v -> %v", st.name, prev, d.UpdatedAt)
		}
		prev = d.UpdatedAt
	}

	got, err := r.GetKBDocument(context.Background(), scope, base.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Author != "bob" || got.Reason != "shared" || got.Title != "Overview 2" || got.Content != "v2" || len(got.Tags) != 0 {
		t.Errorf("stored doc = %+v", got)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at moved: %v -> %v", created.CreatedAt, got.CreatedAt)
	}
	want := []string{"1:create", "2:update", "3:update", "4:update", "5:update", "6:update"}
	if v := kbVersions(t, r, got.ID); !slices.Equal(v, want) {
		t.Errorf("versions = %v, want %v", v, want)
	}
}

func TestKBUpsertExpectVersion(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()
	scope := newScope(KBScopeTeam)
	w := KBDocWrite{Path: "cas.md", Content: "one", By: "a"}

	t.Run("missing doc with an expected version conflicts", func(t *testing.T) {
		w := w
		w.ExpectVersion = 1
		if _, _, err := r.UpsertKBDocument(ctx, scope, w); !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if _, err := r.GetKBDocument(ctx, scope, w.Path); !errors.Is(err, ErrNotFound) {
			t.Fatalf("doc was created: %v", err)
		}
	})

	mustUpsert(t, r, scope, w) // version 1

	t.Run("matching version writes", func(t *testing.T) {
		w := w
		w.Content, w.ExpectVersion = "two", 1
		d, changed := mustUpsert(t, r, scope, w)
		if !changed || d.Version != 2 {
			t.Fatalf("changed=%v version=%d", changed, d.Version)
		}
	})
	t.Run("stale version conflicts and writes nothing", func(t *testing.T) {
		w := w
		w.Content, w.ExpectVersion = "stale", 1
		if _, _, err := r.UpsertKBDocument(ctx, scope, w); !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		d, err := r.GetKBDocument(ctx, scope, w.Path)
		if err != nil || d.Content != "two" || d.Version != 2 {
			t.Fatalf("doc after conflict = %+v, %v", d, err)
		}
	})
}

func TestKBSoftDeleteAndRestore(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()
	scope := newScope(KBScopeProject)
	keep, _ := mustUpsert(t, r, scope, KBDocWrite{Path: "keep.md", Content: "k", By: "a"})
	gone, _ := mustUpsert(t, r, scope, KBDocWrite{Path: "gone.md", Content: "g", By: "a"})

	live := func() (int, []string) {
		t.Helper()
		n, err := r.CountKBDocuments(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		docs, err := r.ListKBDocuments(ctx, scope, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, d := range docs {
			paths = append(paths, d.Path)
		}
		sort.Strings(paths)
		return n, paths
	}

	if n, p := live(); n != 2 || !slices.Equal(p, []string{"gone.md", "keep.md"}) {
		t.Fatalf("before delete: n=%d paths=%v", n, p)
	}

	if err := r.DeleteKBDocument(ctx, scope, "gone.md", "carol"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, p := live(); n != 1 || !slices.Equal(p, []string{"keep.md"}) {
		t.Errorf("after delete: n=%d paths=%v", n, p)
	}
	if _, err := r.GetKBDocument(ctx, scope, "gone.md"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get deleted doc: err = %v, want ErrNotFound", err)
	}
	if err := r.DeleteKBDocument(ctx, scope, "gone.md", "carol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
	if err := r.DeleteKBDocument(ctx, scope, "never.md", "carol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing: err = %v, want ErrNotFound", err)
	}
	if _, err := r.RestoreKBDocument(ctx, scope, "keep.md", "carol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("restore live doc: err = %v, want ErrNotFound", err)
	}
	if _, err := r.UpdateKBAnnotations(ctx, scope, "gone.md", func(a []domain.KBAnnotation) ([]domain.KBAnnotation, error) {
		return a, nil
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("annotate deleted doc: err = %v, want ErrNotFound", err)
	}

	restored, err := r.RestoreKBDocument(ctx, scope, "gone.md", "dave")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.DeletedAt != nil || restored.Version != 3 || restored.Content != "g" {
		t.Errorf("restored = %+v", restored)
	}
	if n, p := live(); n != 2 || !slices.Equal(p, []string{"gone.md", "keep.md"}) {
		t.Errorf("after restore: n=%d paths=%v", n, p)
	}
	if v := kbVersions(t, r, gone.ID); !slices.Equal(v, []string{"1:create", "2:delete", "3:restore"}) {
		t.Errorf("versions = %v", v)
	}

	// Writing a deleted path revives the same row with a 'create' version.
	if err := r.DeleteKBDocument(ctx, scope, "gone.md", "carol"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	revived, changed := mustUpsert(t, r, scope, KBDocWrite{Path: "gone.md", Content: "g", By: "erin"})
	if !changed || revived.ID != gone.ID || revived.DeletedAt != nil || revived.Version != 5 {
		t.Errorf("revive: changed=%v doc=%+v", changed, revived)
	}
	if !revived.UpdatedAt.After(restored.UpdatedAt) {
		t.Errorf("revive kept updated_at %v, want after %v", revived.UpdatedAt, restored.UpdatedAt)
	}
	if v := kbVersions(t, r, gone.ID); !slices.Equal(v, []string{"1:create", "2:delete", "3:restore", "4:delete", "5:create"}) {
		t.Errorf("versions after revive = %v", v)
	}
	if v := kbVersions(t, r, keep.ID); !slices.Equal(v, []string{"1:create"}) {
		t.Errorf("untouched doc versions = %v", v)
	}
}

func TestKBListOrderAndPaging(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()
	scope := newScope(KBScopeOrg)
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		mustUpsert(t, r, scope, KBDocWrite{Path: p, Content: p})
	}
	mustUpsert(t, r, scope, KBDocWrite{Path: "a.md", Content: "newer"}) // a is now most recent

	tests := []struct {
		limit, offset int
		want          []string
	}{
		{0, 0, []string{"a.md", "c.md", "b.md"}},
		{2, 0, []string{"a.md", "c.md"}},
		{2, 2, []string{"b.md"}},
		{0, -5, []string{"a.md", "c.md", "b.md"}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("limit=%d,offset=%d", tt.limit, tt.offset), func(t *testing.T) {
			docs, err := r.ListKBDocuments(ctx, scope, tt.limit, tt.offset)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, d := range docs {
				got = append(got, d.Path)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKBInsertIfMissing(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()
	scope := newScope(KBScopeProject)
	updatedAt := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	createdAt := updatedAt.Add(-48 * time.Hour)
	notes := []domain.KBAnnotation{{Author: "x", Text: "old note", CreatedAt: 42}}

	t.Run("inserts with the given timestamps and annotations", func(t *testing.T) {
		ok, err := r.InsertKBDocumentIfMissing(ctx, scope, KBDocument{Path: "old.md", Title: "Old", Tags: []string{"t"},
			Author: "x", Content: "from typesense", Annotations: notes, CreatedAt: createdAt, UpdatedAt: updatedAt}, "backfill")
		if err != nil || !ok {
			t.Fatalf("inserted=%v err=%v", ok, err)
		}
		d, err := r.GetKBDocument(ctx, scope, "old.md")
		if err != nil {
			t.Fatal(err)
		}
		if !d.UpdatedAt.Equal(updatedAt) || !d.CreatedAt.Equal(createdAt) {
			t.Errorf("timestamps = %v / %v, want %v / %v", d.CreatedAt, d.UpdatedAt, createdAt, updatedAt)
		}
		if d.Version != 1 || d.Content != "from typesense" || !slices.Equal(d.Annotations, notes) {
			t.Errorf("doc = %+v", d)
		}
		if v := kbVersions(t, r, d.ID); !slices.Equal(v, []string{"1:backfill"}) {
			t.Errorf("versions = %v", v)
		}
	})

	t.Run("never overwrites a live doc", func(t *testing.T) {
		live, _ := mustUpsert(t, r, scope, KBDocWrite{Path: "live.md", Content: "postgres wins"})
		ok, err := r.InsertKBDocumentIfMissing(ctx, scope, KBDocument{Path: "live.md", Content: "stale", UpdatedAt: updatedAt}, "backfill")
		if err != nil || ok {
			t.Fatalf("inserted=%v err=%v, want false/nil", ok, err)
		}
		d, err := r.GetKBDocument(ctx, scope, "live.md")
		if err != nil || d.Content != "postgres wins" || !d.UpdatedAt.Equal(live.UpdatedAt) {
			t.Errorf("doc = %+v, %v", d, err)
		}
	})

	t.Run("skips a soft-deleted path", func(t *testing.T) {
		mustUpsert(t, r, scope, KBDocWrite{Path: "deleted.md", Content: "x"})
		if err := r.DeleteKBDocument(ctx, scope, "deleted.md", "a"); err != nil {
			t.Fatal(err)
		}
		ok, err := r.InsertKBDocumentIfMissing(ctx, scope, KBDocument{Path: "deleted.md", Content: "resurrect"}, "backfill")
		if err != nil || ok {
			t.Fatalf("inserted=%v err=%v, want false/nil", ok, err)
		}
		if _, err := r.GetKBDocument(ctx, scope, "deleted.md"); !errors.Is(err, ErrNotFound) {
			t.Errorf("deleted doc came back: %v", err)
		}
	})

	t.Run("zero timestamps default to now", func(t *testing.T) {
		before := time.Now().Add(-time.Minute)
		if ok, err := r.InsertKBDocumentIfMissing(ctx, scope, KBDocument{Path: "fresh.md"}, "backfill"); err != nil || !ok {
			t.Fatalf("inserted=%v err=%v", ok, err)
		}
		d, err := r.GetKBDocument(ctx, scope, "fresh.md")
		if err != nil {
			t.Fatal(err)
		}
		if d.UpdatedAt.Before(before) || !d.CreatedAt.Equal(d.UpdatedAt) {
			t.Errorf("timestamps = %v / %v", d.CreatedAt, d.UpdatedAt)
		}
	})
}

func TestKBAnnotations(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()
	scope := newScope(KBScopeTeam)
	doc, _ := mustUpsert(t, r, scope, KBDocWrite{Path: "notes.md", Content: "c"})

	t.Run("round-trip", func(t *testing.T) {
		want := []domain.KBAnnotation{
			{Author: "a", Text: "first", CreatedAt: 1},
			{Author: "b", Text: "second", CreatedAt: 2, IsResolved: true, ResolvedBy: "c", ResolvedAt: 3},
		}
		if _, err := r.SaveKBAnnotations(ctx, scope, "notes.md", want); err != nil {
			t.Fatal(err)
		}
		d, err := r.GetKBDocument(ctx, scope, "notes.md")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(d.Annotations, want) {
			t.Errorf("annotations = %+v, want %+v", d.Annotations, want)
		}
		if d.Version != doc.Version || !d.UpdatedAt.Equal(doc.UpdatedAt) {
			t.Errorf("annotations moved version/updated_at: %d %v", d.Version, d.UpdatedAt)
		}
		if v := kbVersions(t, r, doc.ID); !slices.Equal(v, []string{"1:create"}) {
			t.Errorf("annotations wrote versions: %v", v)
		}
	})

	t.Run("nil clears to an empty list", func(t *testing.T) {
		d, err := r.SaveKBAnnotations(ctx, scope, "notes.md", nil)
		if err != nil || d.Annotations == nil || len(d.Annotations) != 0 {
			t.Fatalf("annotations = %#v, err %v", d.Annotations, err)
		}
	})

	t.Run("an error from fn writes nothing", func(t *testing.T) {
		if _, err := r.SaveKBAnnotations(ctx, scope, "notes.md", []domain.KBAnnotation{{Text: "kept"}}); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("boom")
		_, err := r.UpdateKBAnnotations(ctx, scope, "notes.md", func(a []domain.KBAnnotation) ([]domain.KBAnnotation, error) {
			return append(a, domain.KBAnnotation{Text: "lost"}), boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		d, _ := r.GetKBDocument(ctx, scope, "notes.md")
		if len(d.Annotations) != 1 || d.Annotations[0].Text != "kept" {
			t.Errorf("annotations = %+v", d.Annotations)
		}
	})

	t.Run("missing doc", func(t *testing.T) {
		if _, err := r.SaveKBAnnotations(ctx, scope, "nope.md", nil); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestKBConcurrentAnnotationsAllLand(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()
	scope := newScope(KBScopeProject)
	doc, _ := mustUpsert(t, r, scope, KBDocWrite{Path: "busy.md", Content: "c"})

	const n = 25
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.UpdateKBAnnotations(ctx, scope, "busy.md", func(a []domain.KBAnnotation) ([]domain.KBAnnotation, error) {
				return append(a, domain.KBAnnotation{Author: "w", Text: fmt.Sprintf("note-%d", i), CreatedAt: int64(i)}), nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("annotate: %v", err)
		}
	}
	d, err := r.GetKBDocument(ctx, scope, "busy.md")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range d.Annotations {
		seen[a.Text] = true
	}
	if len(d.Annotations) != n || len(seen) != n {
		t.Errorf("got %d annotations (%d distinct), want %d", len(d.Annotations), len(seen), n)
	}
	if d.Version != doc.Version {
		t.Errorf("version moved to %d", d.Version)
	}
}

// TestKBConcurrentCreateSamePath races writers creating the same new path.
// All but one of them find no row, then lose the INSERT ... ON CONFLICT DO
// NOTHING and fall back to re-locking the winner's row and updating it. None
// may fail, and every write must land as its own version on one row.
//
// Left to the scheduler the first writer usually commits before the rest
// reach their SELECT, so the fallback is hit only now and then. To make it
// certain, a gate transaction on its own connection holds kb_documents in
// SHARE mode: each writer's SELECT ... FOR UPDATE (ROW SHARE) gets past it,
// but its INSERT (ROW EXCLUSIVE) waits. Once every writer that has a pool
// connection is waiting there, the gate rolls back and they all insert at
// once: one wins and the others take the fallback.
func TestKBConcurrentCreateSamePath(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()
	scope := newScope(KBScopeProject)
	const path = "contended.md"

	gateConn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect gate: %v", err)
	}
	defer gateConn.Close(ctx)
	gate, err := gateConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `LOCK TABLE kb_documents IN SHARE MODE`); err != nil {
		t.Fatalf("lock kb_documents: %v", err)
	}
	// Poll from a third connection, outside any transaction: inside one,
	// pg_stat_activity is a snapshot taken once and never changes.
	pollConn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect poller: %v", err)
	}
	defer pollConn.Close(ctx)

	const n = 16
	var wg sync.WaitGroup
	type result struct {
		doc     *KBDocument
		changed bool
		err     error
	}
	results := make(chan result, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, changed, err := r.UpsertKBDocument(ctx, scope, KBDocWrite{
				Path: path, Title: "Contended", Content: fmt.Sprintf("content from writer %d", i), By: fmt.Sprintf("w%d", i),
			})
			results <- result{d, changed, err}
		}()
	}

	// Wait until every writer holding a pool connection is blocked on its
	// INSERT; the rest are waiting for a connection and run after the gate.
	queued := min(n, int(r.db.Config().MaxConns))
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := pollConn.QueryRow(ctx, `SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query LIKE 'INSERT INTO kb_documents %'`).Scan(&waiting); err != nil {
			t.Fatalf("count waiting writers: %v", err)
		}
		if waiting >= queued {
			break
		}
		if time.Now().After(deadline) {
			gate.Rollback(ctx)
			wg.Wait()
			t.Fatalf("only %d of %d writers reached the INSERT before the deadline", waiting, queued)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := gate.Rollback(ctx); err != nil {
		t.Fatalf("release gate: %v", err)
	}
	wg.Wait()
	close(results)

	var got []int
	for res := range results {
		if res.err != nil {
			t.Fatalf("concurrent create: %v", res.err)
		}
		if !res.changed {
			t.Errorf("a write with new content reported no change (version %d)", res.doc.Version)
		}
		got = append(got, res.doc.Version)
	}
	sort.Ints(got)
	for i, v := range got {
		if v != i+1 {
			t.Fatalf("returned versions %v, want each of 1..%d exactly once", got, n)
		}
	}

	var rowCount int
	if err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM kb_documents WHERE scope_kind = $1 AND scope_id = $2 AND path = $3`,
		scope.Kind, scope.ID, path).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 1 {
		t.Fatalf("got %d kb_documents rows for %s, want 1", rowCount, path)
	}

	d, err := r.GetKBDocument(ctx, scope, path)
	if err != nil {
		t.Fatal(err)
	}
	versions := kbVersions(t, r, d.ID)
	if d.Version != len(versions) || d.Version != n {
		t.Fatalf("doc version %d, %d version rows, want both %d", d.Version, len(versions), n)
	}
	for i, v := range versions {
		want := fmt.Sprintf("%d:update", i+1)
		if i == 0 {
			want = "1:create"
		}
		if v != want {
			t.Fatalf("version rows %v, want 1:create then 2..%d:update", versions, n)
		}
	}

	var latest string
	if err := r.db.QueryRow(ctx, `SELECT content FROM kb_document_versions WHERE document_id = $1 ORDER BY version DESC LIMIT 1`,
		d.ID).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if d.Content != latest {
		t.Errorf("stored content %q, latest version row has %q", d.Content, latest)
	}
}

func TestKBScopesDontLeak(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()
	id := 1_000_000 + rand.IntN(1_000_000_000)
	project := KBScope{Kind: KBScopeProject, ID: id}
	team := KBScope{Kind: KBScopeTeam, ID: id} // same id, other kind
	other := KBScope{Kind: KBScopeProject, ID: id + 1}

	mustUpsert(t, r, project, KBDocWrite{Path: "same.md", Content: "project"})
	mustUpsert(t, r, team, KBDocWrite{Path: "same.md", Content: "team"})
	mustUpsert(t, r, team, KBDocWrite{Path: "team-only.md", Content: "t"})

	for _, tc := range []struct {
		scope   KBScope
		count   int
		content string // of same.md; "" means absent
	}{
		{project, 1, "project"},
		{team, 2, "team"},
		{other, 0, ""},
	} {
		t.Run(tc.scope.String(), func(t *testing.T) {
			n, err := r.CountKBDocuments(ctx, tc.scope)
			if err != nil || n != tc.count {
				t.Errorf("count = %d, %v; want %d", n, err, tc.count)
			}
			docs, err := r.ListKBDocuments(ctx, tc.scope, 0, 0)
			if err != nil || len(docs) != tc.count {
				t.Errorf("list = %d docs, %v; want %d", len(docs), err, tc.count)
			}
			for _, d := range docs {
				if d.ScopeKind != tc.scope.Kind || d.ScopeID != tc.scope.ID {
					t.Errorf("listed a doc from %s %d", d.ScopeKind, d.ScopeID)
				}
			}
			d, err := r.GetKBDocument(ctx, tc.scope, "same.md")
			switch {
			case tc.content == "" && !errors.Is(err, ErrNotFound):
				t.Errorf("get: err = %v, want ErrNotFound", err)
			case tc.content != "" && (err != nil || d.Content != tc.content):
				t.Errorf("get: %+v, %v; want content %q", d, err, tc.content)
			}
		})
	}

	// Deleting in one scope leaves the same path in another alone.
	if err := r.DeleteKBDocument(ctx, team, "same.md", "a"); err != nil {
		t.Fatal(err)
	}
	if d, err := r.GetKBDocument(ctx, project, "same.md"); err != nil || d.Content != "project" {
		t.Errorf("project doc after team delete: %+v, %v", d, err)
	}
	if err := r.DeleteKBDocument(ctx, other, "same.md", "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete in empty scope: err = %v", err)
	}
}

func TestKBStreamVisitsEveryLiveDoc(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()
	scope := newScope(KBScopeOrg)
	const total = 300
	want := map[string]bool{}
	for i := range total {
		p := fmt.Sprintf("doc-%03d.md", i)
		mustUpsert(t, r, scope, KBDocWrite{Path: p, Content: p})
		want[p] = true
	}
	for i := 0; i < total; i += 50 { // 6 deleted
		p := fmt.Sprintf("doc-%03d.md", i)
		if err := r.DeleteKBDocument(ctx, scope, p, "a"); err != nil {
			t.Fatal(err)
		}
		delete(want, p)
	}
	mustUpsert(t, r, newScope(KBScopeOrg), KBDocWrite{Path: "elsewhere.md"})

	seen := map[string]bool{}
	var lastID int64
	err := r.StreamKBDocuments(ctx, scope, func(d KBDocument) error {
		if seen[d.Path] {
			return fmt.Errorf("%s streamed twice", d.Path)
		}
		if d.ID <= lastID {
			return fmt.Errorf("not in id order: %d after %d", d.ID, lastID)
		}
		if d.Content != d.Path {
			return fmt.Errorf("%s: content %q", d.Path, d.Content)
		}
		lastID = d.ID
		seen[d.Path] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(want) {
		t.Errorf("streamed %d docs, want %d", len(seen), len(want))
	}
	for p := range want {
		if !seen[p] {
			t.Errorf("%s not streamed", p)
		}
	}

	t.Run("an error from fn stops the stream", func(t *testing.T) {
		stop := errors.New("stop")
		calls := 0
		err := r.StreamKBDocuments(ctx, scope, func(KBDocument) error {
			calls++
			if calls == 10 {
				return stop
			}
			return nil
		})
		if !errors.Is(err, stop) || calls != 10 {
			t.Errorf("err = %v after %d calls", err, calls)
		}
	})
}

func TestKBBackfillTracking(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()
	scope := newScope(KBScopeProject)

	get := func() *KBBackfill {
		t.Helper()
		b, err := r.GetKBBackfill(ctx, scope)
		if err != nil {
			t.Fatalf("get backfill: %v", err)
		}
		return b
	}
	complete := func() bool {
		t.Helper()
		ok, err := r.IsKBBackfillComplete(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if _, err := r.GetKBBackfill(ctx, scope); !errors.Is(err, ErrNotFound) {
		t.Fatalf("never started: err = %v, want ErrNotFound", err)
	}
	if complete() {
		t.Fatal("complete before starting")
	}

	if err := r.StartKBBackfill(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if b := get(); b.CompletedAt != nil || b.LastError != "" || complete() {
		t.Errorf("after start: %+v", b)
	}

	if err := r.FailKBBackfill(ctx, scope, "typesense timeout"); err != nil {
		t.Fatal(err)
	}
	if b := get(); b.LastError != "typesense timeout" || b.CompletedAt != nil || complete() {
		t.Errorf("after fail: %+v", b)
	}

	if err := r.StartKBBackfill(ctx, scope); err != nil { // retry clears the error
		t.Fatal(err)
	}
	if b := get(); b.LastError != "" {
		t.Errorf("restart kept the error: %+v", b)
	}

	if err := r.CompleteKBBackfill(ctx, scope, 17); err != nil {
		t.Fatal(err)
	}
	b := get()
	if b.CompletedAt == nil || b.DocCount != 17 || b.LastError != "" || !complete() {
		t.Errorf("after complete: %+v", b)
	}

	// A later start or failure leaves the earlier completion in place.
	if err := r.StartKBBackfill(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if err := r.FailKBBackfill(ctx, scope, "again"); err != nil {
		t.Fatal(err)
	}
	if b2 := get(); b2.CompletedAt == nil || !b2.CompletedAt.Equal(*b.CompletedAt) || !complete() {
		t.Errorf("completion lost: %+v", b2)
	}

	t.Run("fail and complete without a start", func(t *testing.T) {
		s := newScope(KBScopeTeam)
		if err := r.FailKBBackfill(ctx, s, "early"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := r.IsKBBackfillComplete(ctx, s); ok {
			t.Error("failed backfill reads as complete")
		}
		s2 := newScope(KBScopeTeam)
		if err := r.CompleteKBBackfill(ctx, s2, 3); err != nil {
			t.Fatal(err)
		}
		if ok, _ := r.IsKBBackfillComplete(ctx, s2); !ok {
			t.Error("completed backfill reads as incomplete")
		}
	})
}
