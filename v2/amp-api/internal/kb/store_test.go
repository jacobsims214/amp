package kb

// Tests for the Postgres-first write path (#1987). They need both stores:
//
//	TEST_DATABASE_URL=postgres://... TEST_TYPESENSE_URL=http://localhost:8108 TEST_TYPESENSE_KEY=test \
//	go test ./internal/kb/... -race -count=1
//
// Migrations are read from cmd/server/migrations (embedded in package main,
// which this package can't import). Each test uses its own random team id,
// so tests and repeated runs don't see each other's rows or collections.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/typesense/typesense-go/v4/typesense/api"
	"github.com/typesense/typesense-go/v4/typesense/api/pointer"

	"github.com/simstech/amp-api/internal/queue"
	"github.com/simstech/amp-api/internal/repository"
)

func storeTestRepo(t *testing.T) *repository.Repo {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the KB store tests against Postgres")
	}
	ctx := context.Background()
	r, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(r.Close)
	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "server", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	var ms []repository.Migration
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		ms = append(ms, repository.Migration{Version: strings.TrimSuffix(filepath.Base(f), ".sql"), SQL: string(b)})
	}
	if err := r.RunMigrations(ctx, ms); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return r
}

func typesenseEnv(t *testing.T) (url, key string) {
	t.Helper()
	url = os.Getenv("TEST_TYPESENSE_URL")
	if url == "" {
		t.Skip("set TEST_TYPESENSE_URL to run against Typesense")
	}
	key = os.Getenv("TEST_TYPESENSE_KEY")
	if key == "" {
		key = "test"
	}
	return url, key
}

// storedService returns a service backed by Postgres and the test Typesense,
// and a fresh team scope whose collection is dropped after the test.
func storedService(t *testing.T) (*Service, *repository.Repo, Scope) {
	t.Helper()
	repo := storeTestRepo(t)
	url, key := typesenseEnv(t)
	s := New(url, key, "", "")
	s.SetStore(repo)
	sc := Team(1_000_000 + rand.IntN(1_000_000_000))
	t.Cleanup(func() { s.Drop(context.Background(), sc) }) //nolint:errcheck
	return s, repo, sc
}

func mustRow(t *testing.T, repo *repository.Repo, sc Scope, path string) *repository.KBDocument {
	t.Helper()
	d, err := repo.GetKBDocument(context.Background(), storeScope(sc), path)
	if err != nil {
		t.Fatalf("postgres row %s: %v", path, err)
	}
	return d
}

// chunkCount counts the index entries (chunks) of path.
func chunkCount(t *testing.T, s *Service, sc Scope, path string) int {
	t.Helper()
	q, perPage := "*", 0
	res, err := s.client.Collection(collectionName(sc)).Documents().Search(context.Background(), &api.SearchCollectionParams{
		Q: &q, QueryBy: pointer.String("title"), FilterBy: pointer.String("path:=" + quoteFilter(path)), PerPage: &perPage,
	})
	if err != nil {
		t.Fatalf("count chunks of %s: %v", path, err)
	}
	return *res.Found
}

func longDoc(paragraphs int, word string) string {
	var b strings.Builder
	for i := 0; i < paragraphs; i++ {
		fmt.Fprintf(&b, "%s paragraph %d %s\n\n", word, i, strings.Repeat(word+" ", 200))
	}
	return b.String()
}

// A write commits to Postgres before Typesense is touched: with Typesense
// unreachable, write, annotate and resolve still succeed and return the
// saved doc, and Postgres holds it.
func TestStoredWriteSurvivesTypesenseDown(t *testing.T) {
	repo := storeTestRepo(t)
	s := New("http://127.0.0.1:1", "x", "", "")
	s.SetStore(repo)
	sc := Team(1_000_000 + rand.IntN(1_000_000_000))
	ctx := context.Background()
	// Backfilled KB: nothing left to read from the index first.
	if err := repo.CompleteKBBackfill(ctx, storeScope(sc), 0); err != nil {
		t.Fatal(err)
	}

	sum, err := s.WriteDoc(ctx, sc, "down.md", "Down", "Written while the index is down.", "me", []string{"t"}, "why", nil)
	if err != nil {
		t.Fatalf("write with typesense down: %v", err)
	}
	if sum.Path != "down.md" || sum.Title != "Down" || sum.Reason != "why" || sum.UpdatedAt == 0 {
		t.Errorf("summary = %+v", sum)
	}
	if _, err := s.AnnotateDoc(ctx, sc, "down.md", "a note", "me"); err != nil {
		t.Fatalf("annotate with typesense down: %v", err)
	}
	doc, err := s.ResolveAnnotation(ctx, sc, "down.md", 0, true, "me")
	if err != nil {
		t.Fatalf("resolve with typesense down: %v", err)
	}
	if len(doc.Annotations) != 1 || !doc.Annotations[0].IsResolved || doc.Content != "Written while the index is down." {
		t.Errorf("resolve returned %+v", doc)
	}
	row := mustRow(t, repo, sc, "down.md")
	if row.Content != "Written while the index is down." || len(row.Annotations) != 1 || !row.Annotations[0].IsResolved {
		t.Errorf("postgres row = %+v", row)
	}
	if _, err := s.AnnotateDoc(ctx, sc, "missing.md", "x", "me"); err == nil || !strings.Contains(err.Error(), "doc not found") {
		t.Errorf("annotate a missing doc: err = %v, want doc not found", err)
	}
}

// A rewrite never deletes the old chunks before the new ones are in, and
// leaves exactly the new version's chunks.
func TestStoredRewriteReplacesChunks(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()

	long := longDoc(4, "alpha")
	if _, err := s.WriteDoc(ctx, sc, "c.md", "C", long, "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	n := chunkCount(t, s, sc, "c.md")
	if n < 3 {
		t.Fatalf("long doc indexed as %d chunks, want several", n)
	}
	if _, err := s.WriteDoc(ctx, sc, "c.md", "C", "short now", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if n := chunkCount(t, s, sc, "c.md"); n != 1 {
		t.Errorf("after shrinking: %d chunks, want 1", n)
	}
	if _, err := s.WriteDoc(ctx, sc, "c.md", "C", longDoc(3, "beta"), "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	doc, err := s.getIndexed(ctx, sc, "c.md")
	if err != nil {
		t.Fatal(err)
	}
	row := mustRow(t, repo, sc, "c.md")
	if doc.Content != row.Content || doc.UpdatedAt != row.UpdatedAt.Unix() {
		t.Errorf("index has content %.20q updated %d; postgres %.20q updated %d", doc.Content, doc.UpdatedAt, row.Content, row.UpdatedAt.Unix())
	}
	if row.Version != 3 {
		t.Errorf("postgres version = %d, want 3", row.Version)
	}
}

func indexTask(t *testing.T, sc Scope, path string) *asynq.Task {
	t.Helper()
	b, err := json.Marshal(queue.IndexDocPayload{ProjectID: sc.projectID(), Scope: sc.Kind, ScopeID: sc.ID, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return asynq.NewTask(queue.TypeKBIndexDoc, b)
}

// Index jobs carry only the path and index the row as it is when they run:
// a late job indexes the newest version, and a job for a deleted row
// removes the doc from the index.
func TestIndexJobReadsCurrentRow(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "j.md", "J", "version one", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	// Committed but not indexed yet (as if its job were still queued).
	if _, _, err := repo.UpsertKBDocument(ctx, storeScope(sc), repository.KBDocWrite{Path: "j.md", Title: "J", Content: "version two", Author: "me"}); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleIndexDocTask(ctx, indexTask(t, sc, "j.md")); err != nil {
		t.Fatal(err)
	}
	doc, err := s.getIndexed(ctx, sc, "j.md")
	if err != nil || doc.Content != "version two" {
		t.Fatalf("after index job: %+v, %v", doc, err)
	}

	if err := repo.DeleteKBDocument(ctx, storeScope(sc), "j.md", "me"); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleIndexDocTask(ctx, indexTask(t, sc, "j.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.getIndexed(ctx, sc, "j.md"); err == nil {
		t.Error("a deleted row is still in the index")
	}
}

// Concurrent annotations on one doc all land, in Postgres and in the index,
// and annotating or resolving never moves updated_at.
func TestStoredConcurrentAnnotations(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "a.md", "A", "Alpha.", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	before := mustRow(t, repo, sc, "a.md")
	time.Sleep(1100 * time.Millisecond) // a moved updated_at would show in unix seconds

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.AnnotateDoc(ctx, sc, "a.md", fmt.Sprintf("note %d", i), "me"); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if _, err := s.ResolveAnnotation(ctx, sc, "a.md", 3, true, "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAnnotation(ctx, sc, "a.md", n, true, "me"); err == nil || strings.Contains(err.Error(), "doc not found") {
		t.Errorf("out-of-range resolve: err = %v, want an index error", err)
	}

	row := mustRow(t, repo, sc, "a.md")
	if len(row.Annotations) != n || !row.Annotations[3].IsResolved {
		t.Errorf("postgres has %d annotations (want %d), #3 resolved=%v", len(row.Annotations), n, len(row.Annotations) > 3 && row.Annotations[3].IsResolved)
	}
	if !row.UpdatedAt.Equal(before.UpdatedAt) || row.Version != before.Version {
		t.Errorf("annotations moved updated_at %v -> %v or version %d -> %d", before.UpdatedAt, row.UpdatedAt, before.Version, row.Version)
	}
	doc, err := s.getIndexed(ctx, sc, "a.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Annotations) != n || !doc.Annotations[3].IsResolved || doc.UpdatedAt != before.UpdatedAt.Unix() {
		t.Errorf("index has %d annotations, updated_at %d; want %d, %d", len(doc.Annotations), doc.UpdatedAt, n, before.UpdatedAt.Unix())
	}
}

// Writing a deleted path again starts the doc over: its old annotations
// were notes on the deleted doc and are cleared.
func TestStoredReviveClearsAnnotations(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "r.md", "R", "first life", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnnotateDoc(ctx, sc, "r.md", "old note", "me"); err != nil {
		t.Fatal(err)
	}
	// A plain rewrite keeps them.
	if _, err := s.WriteDoc(ctx, sc, "r.md", "R", "first life, edited", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if row := mustRow(t, repo, sc, "r.md"); len(row.Annotations) != 1 {
		t.Fatalf("rewrite kept %d annotations, want 1", len(row.Annotations))
	}
	if err := repo.DeleteKBDocument(ctx, storeScope(sc), "r.md", "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteDoc(ctx, sc, "r.md", "R", "second life", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if row := mustRow(t, repo, sc, "r.md"); len(row.Annotations) != 0 || row.Content != "second life" {
		t.Errorf("revived row = %+v, want no annotations", row)
	}
	doc, err := s.getIndexed(ctx, sc, "r.md")
	if err != nil || len(doc.Annotations) != 0 {
		t.Errorf("revived index doc = %+v, %v", doc, err)
	}
}

// Until a KB is backfilled, a doc only the index holds is copied into
// Postgres (with its annotations and date) before its first write or
// annotation, so neither loses what the index had.
func TestStoredAdoptsIndexOnlyDoc(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	url, key := typesenseEnv(t)
	legacy := New(url, key, "", "") // no store: Typesense only, as before
	if _, err := legacy.WriteDoc(ctx, sc, "old.md", "Old", "indexed only", "me", []string{"x"}, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.AnnotateDoc(ctx, sc, "old.md", "kept note", "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.WriteDoc(ctx, sc, "old2.md", "Old2", "indexed only too", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.AnnotateDoc(ctx, sc, "old2.md", "first", "me"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.WriteDoc(ctx, sc, "old.md", "Old", "rewritten", "me", []string{"x"}, "", nil); err != nil {
		t.Fatal(err)
	}
	row := mustRow(t, repo, sc, "old.md")
	if row.Content != "rewritten" || len(row.Annotations) != 1 || row.Annotations[0].Text != "kept note" || row.Version != 2 {
		t.Errorf("rewrite of an index-only doc: %+v", row)
	}
	if _, err := s.AnnotateDoc(ctx, sc, "old2.md", "second", "me"); err != nil {
		t.Fatal(err)
	}
	row = mustRow(t, repo, sc, "old2.md")
	if len(row.Annotations) != 2 || row.Annotations[0].Text != "first" || row.Content != "indexed only too" {
		t.Errorf("annotate of an index-only doc: %+v", row)
	}
}

// A kb:write_doc task queued by an older amp-api holds the only copy of
// that write: draining it saves it to Postgres, then indexes it.
func TestLegacyWriteTaskIsSaved(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	b, err := json.Marshal(queue.WriteDocPayload{Scope: sc.Kind, ScopeID: sc.ID, Path: "q.md", Title: "Q", Content: "queued before the upgrade", Author: "me", Tags: []string{"t"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.HandleWriteDocTask(ctx, asynq.NewTask(queue.TypeKBWriteDoc, b)); err != nil {
		t.Fatal(err)
	}
	if row := mustRow(t, repo, sc, "q.md"); row.Content != "queued before the upgrade" {
		t.Errorf("postgres row = %+v", row)
	}
	doc, err := s.getIndexed(ctx, sc, "q.md")
	if err != nil || doc.Content != "queued before the upgrade" {
		t.Errorf("index doc = %+v, %v", doc, err)
	}

	// A queued write for a doc deleted since doesn't bring it back.
	if err := repo.DeleteKBDocument(ctx, storeScope(sc), "q.md", "me"); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleWriteDocTask(ctx, asynq.NewTask(queue.TypeKBWriteDoc, b)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetKBDocument(ctx, storeScope(sc), "q.md"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("deleted doc after a stale queued write: err = %v, want not found", err)
	}
}

// A reindex of a stored KB re-indexes from Postgres and keeps each doc's
// own updated_at.
func TestStoredReindexKeepsUpdatedAt(t *testing.T) {
	s, repo, sc := storedService(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "d.md", "D", "Delta.", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnnotateDoc(ctx, sc, "d.md", "note", "me"); err != nil {
		t.Fatal(err)
	}
	row := mustRow(t, repo, sc, "d.md")
	time.Sleep(1100 * time.Millisecond)
	if err := s.reindex(ctx, sc, true); err != nil {
		t.Fatal(err)
	}
	doc, err := s.getIndexed(ctx, sc, "d.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.UpdatedAt != row.UpdatedAt.Unix() || len(doc.Annotations) != 1 {
		t.Errorf("after reindex: updated_at %d (want %d), %d annotations", doc.UpdatedAt, row.UpdatedAt.Unix(), len(doc.Annotations))
	}
}
