package kb

// Tests for dropping and restoring a whole stored KB (#1991): a team, org or
// project deletion trashes the KB's docs, Drop takes the reindex lock, and
// RestoreKB brings a dropped KB back within the trash period. They need
// Postgres and Typesense; see store_test.go for the environment.

import (
	"context"
	"testing"
	"time"
)

// waitNoCollections waits (bounded) until nothing is left under sc's base
// name: no alias, no real collection, no build.
func waitNoCollections(t *testing.T, s *Service, sc Scope) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		kc := kbCols(t, s, sc)
		if kc.alias == "" && !kc.real && len(kc.builds) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("collections still there: %+v", kc)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func shortDropWait(t *testing.T) {
	t.Helper()
	wait, poll := dropLockWait, dropLockPoll
	dropLockWait, dropLockPoll = 20*time.Second, 20*time.Millisecond
	t.Cleanup(func() { dropLockWait, dropLockPoll = wait, poll })
}

// Drop takes the reindex lock. With the lock held by someone else, it still
// moves the docs to the trash at once, but leaves the collections until the
// lock is released, then drops them.
func TestDropWaitsForReindexLock(t *testing.T) {
	shortDropWait(t)
	s, repo, sc := storedService(t)
	ctx := context.Background()
	for _, p := range []string{"a.md", "b.md"} {
		if _, err := s.WriteDoc(ctx, sc, p, p, "content of "+p, "me", nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	release, err := s.beginReindex(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Drop(ctx, sc); err != nil {
		t.Fatalf("Drop with the lock held: %v", err)
	}
	trashed, err := repo.ListTrashedKBPaths(ctx, storeScope(sc), dropDeletedBy)
	if err != nil || len(trashed) != 2 {
		t.Fatalf("trash after Drop = %v, %v; want both docs", trashed, err)
	}
	if n, _ := s.CountDocs(ctx, sc); n != 0 {
		t.Errorf("CountDocs after Drop = %d, want 0", n)
	}
	time.Sleep(100 * time.Millisecond) // several polls
	if kc := kbCols(t, s, sc); !kc.real {
		t.Fatalf("collection dropped while the reindex lock was held: %+v", kc)
	}
	release()
	waitNoCollections(t, s, sc)
}

// A reindex running while its team is deleted can't leave a build or an
// alias behind: Drop (here called between the build and the swap) waits for
// the reindex to end and then drops everything, and no doc comes back.
func TestDropDuringReindexLeavesNoCollection(t *testing.T) {
	shortDropWait(t)
	s, repo, sc := storedService(t)
	ctx := context.Background()
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		if _, err := s.WriteDoc(ctx, sc, p, p, "content of "+p, "me", nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	dropped := make(chan error, 1)
	s.reindexHook = func(hctx context.Context, stage string) {
		if stage == "built" {
			dropped <- s.Drop(hctx, sc)
		}
	}
	rerr := s.Reindex(ctx, sc)
	s.reindexHook = nil
	select {
	case err := <-dropped:
		if err != nil {
			t.Fatalf("Drop during reindex: %v", err)
		}
	default:
		t.Fatal("the reindex never reached the built stage")
	}
	// The reindex may finish (swapping in a build that its catch-up emptied)
	// or fail its verification; either way nothing may survive the drop.
	t.Logf("reindex during drop returned %v", rerr)
	waitNoCollections(t, s, sc)
	if n, _ := s.CountDocs(ctx, sc); n != 0 {
		t.Errorf("CountDocs after drop = %d, want 0", n)
	}
	if trashed, _ := repo.ListTrashedKBPaths(ctx, storeScope(sc), dropDeletedBy); len(trashed) != 3 {
		t.Errorf("trash = %v, want all 3 docs", trashed)
	}
}

// Deleting the owner keeps the KB in Postgres (soft-deleted); restoring it
// within 30 days brings back exactly the docs the drop trashed, with their
// annotations, searchable again; the purge removes dropped docs only after
// 30 days.
func TestRestoreKBAfterDrop(t *testing.T) {
	s, repo, sc := storedService(t)
	db := rawDB(t)
	ctx := context.Background()
	for _, p := range []string{"keep.md", "old.md", "gone.md"} {
		if _, err := s.WriteDoc(ctx, sc, p, p, "zebrafish notes in "+p, "me", nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AnnotateDoc(ctx, sc, "keep.md", "still true", "you"); err != nil {
		t.Fatal(err)
	}
	// Deleted on its own before the drop: restoring the KB must not revive it.
	if err := s.DeleteDoc(ctx, sc, "gone.md", "me"); err != nil {
		t.Fatal(err)
	}
	if err := s.Drop(ctx, sc); err != nil {
		t.Fatal(err)
	}
	waitNoCollections(t, s, sc)
	for _, p := range []string{"keep.md", "old.md", "gone.md"} {
		if deleted, exists := deletedRow(t, db, sc, p); !exists || !deleted {
			t.Errorf("%s after drop: exists=%v deleted=%v; want a soft-deleted row", p, exists, deleted)
		}
	}

	// old.md was dropped 31 days ago (as if the team had been deleted then):
	// the purge removes it, and keep.md (dropped just now) survives.
	if _, err := db.Exec(ctx, `UPDATE kb_documents SET deleted_at = NOW() - INTERVAL '31 days'
		WHERE scope_kind = $1 AND scope_id = $2 AND path = 'old.md'`, sc.Kind, sc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PurgeTrash(ctx); err != nil {
		t.Fatal(err)
	}
	if _, exists := deletedRow(t, db, sc, "old.md"); exists {
		t.Error("a doc dropped 31 days ago survived the purge")
	}
	if deleted, exists := deletedRow(t, db, sc, "keep.md"); !exists || !deleted {
		t.Error("a doc dropped just now was purged")
	}

	n, err := s.RestoreKB(ctx, sc, "admin")
	if err != nil || n != 1 {
		t.Fatalf("RestoreKB = %d, %v; want 1 doc", n, err)
	}
	d, err := s.GetDoc(ctx, sc, "keep.md")
	if err != nil || d.Content != "zebrafish notes in keep.md" || len(d.Annotations) != 1 || d.Annotations[0].Text != "still true" {
		t.Fatalf("restored doc = %+v, %v", d, err)
	}
	if got := searchPaths(t, s, sc, "zebrafish"); len(got) != 1 || got[0] != "keep.md" {
		t.Errorf("search after restore = %v, want [keep.md]", got)
	}
	if deleted, _ := deletedRow(t, db, sc, "gone.md"); !deleted {
		t.Error("RestoreKB revived a doc deleted on its own before the drop")
	}
	if n, _ := s.CountDocs(ctx, sc); n != 1 {
		t.Errorf("CountDocs after restore = %d, want 1", n)
	}
	// Restoring again finds nothing left to restore.
	if n, err := s.RestoreKB(ctx, sc, "admin"); err != nil || n != 0 {
		t.Errorf("second RestoreKB = %d, %v", n, err)
	}
}

// The global KB keeps no deleted docs, so it can't be restored.
func TestRestoreKBGlobalRefused(t *testing.T) {
	s, _, _ := storedService(t)
	if _, err := s.RestoreKB(context.Background(), Global(), "admin"); err == nil {
		t.Fatal("RestoreKB of the global KB succeeded")
	}
}
