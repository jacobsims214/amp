package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/kb"
)

// amp_kb_reindex is single-flight per KB across replicas: while another
// replica holds a KB's reindex lock, a call starts nothing and says so, and
// once it's free a call starts one that leaves every doc in place.
func TestKB_ReindexSingleFlight(t *testing.T) {
	s := setupMCP(t)
	ctx := context.Background()
	key := fmt.Sprintf("kb_reindex:kb_%d", s.projectID)

	// The advisory lock is per session: a second taker (another replica's
	// pool connection) is refused until the first releases it.
	release, ok, err := s.repo.TryNamedLock(ctx, key)
	if err != nil || !ok {
		t.Fatalf("first lock: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.repo.TryNamedLock(ctx, key); err != nil || ok {
		t.Fatalf("second lock while held: ok=%v err=%v, want refused", ok, err)
	}
	otherKB, ok, err := s.repo.TryNamedLock(ctx, key+"0")
	if err != nil || !ok {
		t.Fatalf("lock for another KB: ok=%v err=%v", ok, err)
	}
	otherKB()

	if s.kb == nil {
		release()
		t.Skip("set TEST_TYPESENSE_URL to run the amp_kb_reindex part against Typesense")
	}
	pid := float64(s.projectID)
	for _, p := range []string{"a.md", "b.md"} {
		s.call(t, "amp_kb_write", map[string]interface{}{"project_id": pid, "path": p, "title": p, "content": "doc " + p, "tags": []interface{}{"x"}})
	}

	// Another replica is reindexing this KB.
	if got := s.call(t, "amp_kb_reindex", map[string]interface{}{"project_id": pid})["reindex"]; got != "already running" {
		t.Fatalf("reindex while another replica runs one: %v, want already running", got)
	}
	release()

	if got := s.call(t, "amp_kb_reindex", map[string]interface{}{"project_id": pid})["reindex"]; got != "started" {
		t.Fatalf("reindex when free: %v, want started", got)
	}
	// Wait (bounded) for the background run to let go of the lock.
	deadline := time.Now().Add(15 * time.Second)
	for {
		rel, ok, err := s.repo.TryNamedLock(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			rel()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reindex still holds the lock after 15s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range []string{"a.md", "b.md"} {
		if _, err := s.kb.GetDoc(ctx, kb.Project(s.projectID), p); err != nil {
			t.Fatalf("%s after reindex: %v", p, err)
		}
	}
}
