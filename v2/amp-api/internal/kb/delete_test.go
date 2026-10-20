package kb

// Tests for delete into the trash, restore and purge of stored KB docs
// (#1988). Same setup as store_test.go: TEST_DATABASE_URL and
// TEST_TYPESENSE_URL.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simstech/amp-api/internal/queue"
	"github.com/simstech/amp-api/internal/repository"
)

func rawDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

// deletedRow reports whether path has a soft-deleted row, and whether it
// has a row at all.
func deletedRow(t *testing.T, db *pgxpool.Pool, sc Scope, path string) (deleted, exists bool) {
	t.Helper()
	var n, d int
	if err := db.QueryRow(context.Background(), `SELECT COUNT(*), COUNT(deleted_at) FROM kb_documents
		WHERE scope_kind = $1 AND scope_id = $2 AND path = $3`, sc.Kind, sc.ID, path).Scan(&n, &d); err != nil {
		t.Fatal(err)
	}
	return d == 1, n == 1
}

func searchPaths(t *testing.T, s *Service, sc Scope, q string) []string {
	t.Helper()
	res, err := s.Search(context.Background(), sc, q, nil, 10, 0, 0)
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	var paths []string
	for _, r := range res {
		paths = append(paths, r.Path)
	}
	return paths
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Deleting a doc leaves a soft-deleted row and removes it from search;
// RestoreDoc brings back the same content and annotations and makes it
// searchable again.
func TestStoredDeleteAndRestore(t *testing.T) {
	s, repo, sc := storedService(t)
	db := rawDB(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "gone.md", "Gone", "The zanzibarquokka doc.", "me", []string{"a", "b"}, "why", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnnotateDoc(ctx, sc, "gone.md", "a kept note", "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAnnotation(ctx, sc, "gone.md", 0, true, "me"); err != nil {
		t.Fatal(err)
	}
	before := mustRow(t, repo, sc, "gone.md")
	if !contains(searchPaths(t, s, sc, "zanzibarquokka"), "gone.md") {
		t.Fatal("doc not searchable before the delete")
	}

	if err := s.DeleteDoc(ctx, sc, "gone.md", "deleter@example.com"); err != nil {
		t.Fatal(err)
	}
	if deleted, _ := deletedRow(t, db, sc, "gone.md"); !deleted {
		t.Fatal("no soft-deleted postgres row after delete")
	}
	var by string
	if err := db.QueryRow(ctx, `SELECT deleted_by FROM kb_documents WHERE scope_kind = $1 AND scope_id = $2 AND path = 'gone.md'`,
		sc.Kind, sc.ID).Scan(&by); err != nil || by != "deleter@example.com" {
		t.Errorf("deleted_by = %q, %v", by, err)
	}
	if _, err := repo.GetKBDocument(ctx, storeScope(sc), "gone.md"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("deleted doc still live in postgres: %v", err)
	}
	if contains(searchPaths(t, s, sc, "zanzibarquokka"), "gone.md") {
		t.Error("deleted doc still in search")
	}
	if n := chunkCount(t, s, sc, "gone.md"); n != 0 {
		t.Errorf("deleted doc has %d chunks in the index", n)
	}
	// Annotating a deleted doc doesn't bring it back.
	if _, err := s.AnnotateDoc(ctx, sc, "gone.md", "x", "me"); err == nil {
		t.Error("annotated a deleted doc")
	}
	// Deleting again (or a doc that never existed) is not an error.
	if err := s.DeleteDoc(ctx, sc, "gone.md", "me"); err != nil {
		t.Errorf("second delete: %v", err)
	}
	if err := s.DeleteDoc(ctx, sc, "never.md", "me"); err != nil {
		t.Errorf("delete of a missing doc: %v", err)
	}

	doc, err := s.RestoreDoc(ctx, sc, "gone.md", "restorer@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Content != before.Content || doc.Title != "Gone" || doc.Reason != "why" || strings.Join(doc.Tags, ",") != "a,b" ||
		len(doc.Annotations) != 1 || doc.Annotations[0].Text != "a kept note" || !doc.Annotations[0].IsResolved {
		t.Errorf("restored doc = %+v", doc)
	}
	after := mustRow(t, repo, sc, "gone.md")
	if after.Content != before.Content || len(after.Annotations) != 1 || after.Version != before.Version+2 {
		t.Errorf("restored row = %+v (version before %d)", after, before.Version)
	}
	if !contains(searchPaths(t, s, sc, "zanzibarquokka"), "gone.md") {
		t.Error("restored doc not searchable")
	}
	idx, err := s.getIndexed(ctx, sc, "gone.md")
	if err != nil || len(idx.Annotations) != 1 || idx.Content != before.Content {
		t.Errorf("restored index doc = %+v, %v", idx, err)
	}

	// Restoring a live doc, or one never deleted, fails.
	if _, err := s.RestoreDoc(ctx, sc, "gone.md", "me"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("restore of a live doc: err = %v", err)
	}
	if _, err := s.RestoreDoc(ctx, sc, "never.md", "me"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("restore of a missing doc: err = %v", err)
	}
	if _, err := s.RestoreDoc(ctx, Global(), "gone.md", "me"); err == nil {
		t.Error("restore in the global KB should fail: it has no trash")
	}
}

// Until a KB is backfilled, deleting a doc only the index holds copies it
// into Postgres first, so it goes to the trash (and can be restored, with
// its annotations) rather than vanishing.
func TestStoredDeleteIndexOnlyDoc(t *testing.T) {
	s, _, sc := storedService(t)
	db := rawDB(t)
	ctx := context.Background()
	url, key := typesenseEnv(t)
	legacy := New(url, key, "", "")
	if _, err := legacy.WriteDoc(ctx, sc, "old.md", "Old", "indexed only", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.AnnotateDoc(ctx, sc, "old.md", "index note", "me"); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteDoc(ctx, sc, "old.md", "me"); err != nil {
		t.Fatal(err)
	}
	if deleted, _ := deletedRow(t, db, sc, "old.md"); !deleted {
		t.Fatal("index-only doc was not moved to the trash")
	}
	if n := chunkCount(t, s, sc, "old.md"); n != 0 {
		t.Errorf("deleted doc has %d chunks in the index", n)
	}
	doc, err := s.RestoreDoc(ctx, sc, "old.md", "me")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Content != "indexed only" || len(doc.Annotations) != 1 || doc.Annotations[0].Text != "index note" {
		t.Errorf("restored = %+v", doc)
	}
}

// A kb:delete_doc task queued by an older amp-api deletes into the trash
// too, so the still-live row can't put the doc back in the index later.
func TestLegacyDeleteTaskSoftDeletes(t *testing.T) {
	s, repo, sc := storedService(t)
	db := rawDB(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "l.md", "L", "legacy delete", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(queue.DeleteDocPayload{Scope: sc.Kind, ScopeID: sc.ID, Path: "l.md"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.HandleDeleteDocTask(ctx, asynq.NewTask(queue.TypeKBDeleteDoc, b)); err != nil {
		t.Fatal(err)
	}
	if deleted, _ := deletedRow(t, db, sc, "l.md"); !deleted {
		t.Error("legacy delete task left the postgres row live")
	}
	if n := chunkCount(t, s, sc, "l.md"); n != 0 {
		t.Errorf("legacy delete left %d chunks", n)
	}
	// A reindex doesn't bring it back.
	if err := s.reindex(ctx, sc, false); err != nil {
		t.Fatal(err)
	}
	if n := chunkCount(t, s, sc, "l.md"); n != 0 {
		t.Errorf("reindex brought back %d chunks of a deleted doc", n)
	}
	if _, err := repo.GetKBDocument(ctx, storeScope(sc), "l.md"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("row live after legacy delete: %v", err)
	}
}

// The trash purge removes KB docs deleted more than 30 days ago (with their
// versions), and leaves younger deleted docs and live docs alone.
func TestPurgeTrashRemovesOldKBDocs(t *testing.T) {
	s, repo, sc := storedService(t)
	db := rawDB(t)
	ctx := context.Background()
	for _, p := range []string{"old.md", "fresh.md", "live.md"} {
		if _, err := s.WriteDoc(ctx, sc, p, p, "content of "+p, "me", nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"old.md", "fresh.md"} {
		if err := s.DeleteDoc(ctx, sc, p, "me"); err != nil {
			t.Fatal(err)
		}
	}
	age := func(path, interval string) {
		if _, err := db.Exec(ctx, `UPDATE kb_documents SET deleted_at = NOW() - $4::interval
			WHERE scope_kind = $1 AND scope_id = $2 AND path = $3`, sc.Kind, sc.ID, path, interval); err != nil {
			t.Fatal(err)
		}
	}
	age("old.md", "31 days")
	age("fresh.md", "29 days")
	var oldID int64
	if err := db.QueryRow(ctx, `SELECT id FROM kb_documents WHERE scope_kind = $1 AND scope_id = $2 AND path = 'old.md'`,
		sc.Kind, sc.ID).Scan(&oldID); err != nil {
		t.Fatal(err)
	}

	n, err := repo.PurgeTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("purged %d rows, want at least the old KB doc", n)
	}
	if _, exists := deletedRow(t, db, sc, "old.md"); exists {
		t.Error("KB doc deleted 31 days ago survived the purge")
	}
	var versions int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM kb_document_versions WHERE document_id = $1`, oldID).Scan(&versions); err != nil || versions != 0 {
		t.Errorf("purged doc left %d versions (%v)", versions, err)
	}
	if deleted, _ := deletedRow(t, db, sc, "fresh.md"); !deleted {
		t.Error("KB doc deleted 29 days ago was purged")
	}
	mustRow(t, repo, sc, "live.md")

	if _, err := s.RestoreDoc(ctx, sc, "old.md", "me"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("restore of a purged doc: err = %v", err)
	}
	if doc, err := s.RestoreDoc(ctx, sc, "fresh.md", "me"); err != nil || doc.Content != "content of fresh.md" {
		t.Errorf("restore of a doc still in the trash: %+v, %v", doc, err)
	}
}
