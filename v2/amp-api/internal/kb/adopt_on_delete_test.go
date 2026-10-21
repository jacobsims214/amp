package kb

// Tests for PR #67 review finding S1 (AMP #2036): on a KB whose backfill is
// complete, deleting a doc only the index holds (an old pod's write during a
// rolling deploy, before the reconciler adopts it), whether by DeleteDoc,
// a legacy kb:delete_doc task or Drop, moves it to the trash instead of
// removing the only copy. Same setup as store_test.go: TEST_DATABASE_URL and
// TEST_TYPESENSE_URL.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/simstech/amp-api/internal/queue"
)

// legacyIndexOnlyDoc writes path straight to Typesense through a service
// with no store (an old pod), with one annotation, so the index holds the
// doc's only copy.
func legacyIndexOnlyDoc(t *testing.T, sc Scope, path, content, note string) {
	t.Helper()
	ctx := context.Background()
	url, key := typesenseEnv(t)
	legacy := New(url, key, "", "")
	if _, err := legacy.WriteDoc(ctx, sc, path, "Old "+path, content, "old@x", []string{"legacy"}, "old pod", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.AnnotateDoc(ctx, sc, path, note, "ann@x"); err != nil {
		t.Fatal(err)
	}
}

// deletedBy is path's deleted_by ("" when live or missing).
func deletedBy(t *testing.T, sc Scope, path string) string {
	t.Helper()
	var by *string
	err := rawDB(t).QueryRow(context.Background(), `SELECT deleted_by FROM kb_documents
		WHERE scope_kind = $1 AND scope_id = $2 AND path = $3`, sc.Kind, sc.ID, path).Scan(&by)
	if err != nil || by == nil {
		return ""
	}
	return *by
}

func checkRestored(t *testing.T, d *Doc, content, note string) {
	t.Helper()
	if d.Content != content || d.Author != "old@x" || d.Reason != "old pod" {
		t.Errorf("restored doc = %q by %q (%q), want %q by old@x", d.Content, d.Author, d.Reason, content)
	}
	if len(d.Annotations) != 1 || d.Annotations[0].Text != note {
		t.Errorf("restored annotations = %+v, want [%q]", d.Annotations, note)
	}
}

func TestDeleteAdoptsIndexOnlyDocAfterBackfill(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	legacyIndexOnlyDoc(t, sc, "old.md", "only the index has this", "index note")
	if done, err := repo.IsKBBackfillComplete(ctx, storeScope(sc)); err != nil || !done {
		t.Fatalf("setup: backfill complete = %v, %v", done, err)
	}

	if err := s.DeleteDoc(ctx, sc, "old.md", "me"); err != nil {
		t.Fatal(err)
	}
	if deleted, exists := deletedRow(t, rawDB(t), sc, "old.md"); !exists || !deleted {
		t.Fatalf("after delete: exists=%v deleted=%v; want a soft-deleted row", exists, deleted)
	}
	if by := deletedBy(t, sc, "old.md"); by != "me" {
		t.Errorf("deleted_by = %q, want me", by)
	}
	if v := versionRows(t, sc, "old.md"); len(v) < 1 || v[0] != "1|backfill|"+deleteAdoptBy+"|only the index has this" {
		t.Errorf("versions = %v, want the adoption first", v)
	}
	if n := chunkCount(t, s, sc, "old.md"); n != 0 {
		t.Errorf("deleted doc has %d chunks in the index", n)
	}
	d, err := s.RestoreDoc(ctx, sc, "old.md", "me")
	if err != nil {
		t.Fatal(err)
	}
	checkRestored(t, d, "only the index has this", "index note")
	if n := chunkCount(t, s, sc, "old.md"); n == 0 {
		t.Error("restored doc not re-indexed")
	}
}

func TestLegacyDeleteTaskAdoptsIndexOnlyDocAfterBackfill(t *testing.T) {
	s, _, sc := backfilledKB(t)
	ctx := context.Background()
	legacyIndexOnlyDoc(t, sc, "l.md", "legacy delete of an index-only doc", "legacy note")

	b, err := json.Marshal(queue.DeleteDocPayload{Scope: sc.Kind, ScopeID: sc.ID, Path: "l.md"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.HandleDeleteDocTask(ctx, asynq.NewTask(queue.TypeKBDeleteDoc, b)); err != nil {
		t.Fatal(err)
	}
	if deleted, exists := deletedRow(t, rawDB(t), sc, "l.md"); !exists || !deleted {
		t.Fatalf("after legacy delete: exists=%v deleted=%v; want a soft-deleted row", exists, deleted)
	}
	if by := deletedBy(t, sc, "l.md"); by != "kb-legacy-delete" {
		t.Errorf("deleted_by = %q, want kb-legacy-delete", by)
	}
	if n := chunkCount(t, s, sc, "l.md"); n != 0 {
		t.Errorf("deleted doc has %d chunks in the index", n)
	}
	d, err := s.RestoreDoc(ctx, sc, "l.md", "me")
	if err != nil {
		t.Fatal(err)
	}
	checkRestored(t, d, "legacy delete of an index-only doc", "legacy note")
}

func TestDropAdoptsIndexOnlyDocAfterBackfill(t *testing.T) {
	s, _, sc := backfilledKB(t)
	if sc.Kind != ScopeTeam {
		t.Fatalf("setup: want a team KB, got %s", sc)
	}
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "keep.md", "Keep", "a stored doc", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	legacyIndexOnlyDoc(t, sc, "old.md", "dropped while index-only", "drop note")

	if err := s.Drop(ctx, sc); err != nil {
		t.Fatal(err)
	}
	waitNoCollections(t, s, sc)
	for _, p := range []string{"keep.md", "old.md"} {
		if by := deletedBy(t, sc, p); by != dropDeletedBy {
			t.Errorf("%s deleted_by = %q, want %s", p, by, dropDeletedBy)
		}
	}
	if v := versionRows(t, sc, "old.md"); len(v) < 1 || v[0] != "1|backfill|"+dropAdoptBy+"|dropped while index-only" {
		t.Errorf("versions = %v, want the adoption first", v)
	}

	n, err := s.RestoreKB(ctx, sc, "admin")
	if err != nil || n != 2 {
		t.Fatalf("RestoreKB = %d, %v; want 2 docs", n, err)
	}
	d, err := s.GetDoc(ctx, sc, "old.md")
	if err != nil {
		t.Fatal(err)
	}
	checkRestored(t, d, "dropped while index-only", "drop note")
	if got := searchPaths(t, s, sc, "dropped"); !contains(got, "old.md") {
		t.Errorf("restored doc not searchable: %v", got)
	}
}

// An old pod's rewrite of a path whose row is soft-deleted is not revived
// by a delete or a Drop: the row stays deleted with its own content, and the
// stale entry is removed from the index.
func TestDeleteAndDropDoNotReviveDeletedPath(t *testing.T) {
	s, _, sc := backfilledKB(t)
	ctx := context.Background()
	if _, err := s.WriteDoc(ctx, sc, "gone.md", "Gone", "the deleted version", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDoc(ctx, sc, "gone.md", "me"); err != nil {
		t.Fatal(err)
	}
	before := rowsSnapshot(t, sc)
	oldPodWrite(t, s, sc, indexedDoc{path: "gone.md", title: "Gone", content: "an old pod's rewrite", updatedAt: time.Now().Unix()})

	if err := s.DeleteDoc(ctx, sc, "gone.md", "you"); err != nil {
		t.Fatal(err)
	}
	if n := chunkCount(t, s, sc, "gone.md"); n != 0 {
		t.Errorf("stale entry left with %d chunks", n)
	}
	oldPodWrite(t, s, sc, indexedDoc{path: "gone.md", title: "Gone", content: "an old pod's rewrite", updatedAt: time.Now().Unix()})
	if err := s.Drop(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if after := rowsSnapshot(t, sc); len(after) != len(before) || after[0] != before[0] {
		t.Errorf("rows changed:\n before %v\n after  %v", before, after)
	}
	if by := deletedBy(t, sc, "gone.md"); by != "me" {
		t.Errorf("deleted_by = %q, want me (untouched)", by)
	}
}
