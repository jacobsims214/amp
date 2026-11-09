package repository

import (
	"context"
	"testing"
	"time"
)

// RecordKBReads (#159): an agent's read sets last_read_at/by and counts once;
// the same reader again inside the throttle writes nothing, even with other
// readers in between (A, B, A); another reader, or the same one after the
// window, counts again. Deleted and missing paths
// are ignored, and existing docs start never-read.
func TestRecordKBReads(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()
	sc := newScope(KBScopeProject)
	for _, p := range []string{"a.md", "b.md", "gone.md"} {
		if _, _, err := r.UpsertKBDocument(ctx, sc, KBDocWrite{Path: p, Title: p, Content: "x", By: "me"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.DeleteKBDocument(ctx, sc, "gone.md", "me"); err != nil {
		t.Fatal(err)
	}
	get := func(p string) *KBDocument {
		t.Helper()
		d, err := r.GetKBDocument(ctx, sc, p)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := get("a.md"); d.LastReadAt != nil || d.LastReadBy != nil || d.ReadCount != 0 {
		t.Fatalf("new doc read stats = %v %v %d; want never read", d.LastReadAt, d.LastReadBy, d.ReadCount)
	}
	before := get("a.md").UpdatedAt

	record := func(by string, throttle time.Duration, paths ...string) int64 {
		t.Helper()
		n, err := r.RecordKBReads(ctx, sc, paths, by, throttle)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := record("agent-1", time.Hour, "a.md", "b.md", "gone.md", "missing.md"); n != 2 {
		t.Fatalf("first read wrote %d rows; want 2 (a, b)", n)
	}
	a := get("a.md")
	if a.LastReadAt == nil || time.Since(*a.LastReadAt) > time.Minute || a.LastReadBy == nil || *a.LastReadBy != "agent-1" || a.ReadCount != 1 {
		t.Fatalf("after read: at=%v by=%v count=%d", a.LastReadAt, a.LastReadBy, a.ReadCount)
	}
	if !a.UpdatedAt.Equal(before) {
		t.Errorf("a read moved updated_at from %v to %v", before, a.UpdatedAt)
	}

	// Throttled: same reader within the window.
	if n := record("agent-1", time.Hour, "a.md"); n != 0 {
		t.Fatalf("repeat read inside the throttle wrote %d rows; want 0", n)
	}
	// Another reader counts, and becomes the last reader.
	if n := record("agent-2", time.Hour, "a.md"); n != 1 {
		t.Fatalf("another reader wrote %d rows; want 1", n)
	}
	// Interleaved readers (A, B, A, B) inside the window: no more writes.
	if n := record("agent-1", time.Hour, "a.md"); n != 0 {
		t.Fatalf("agent-1 again after agent-2, inside the throttle, wrote %d rows; want 0", n)
	}
	if n := record("agent-2", time.Hour, "a.md"); n != 0 {
		t.Fatalf("agent-2 again after agent-1, inside the throttle, wrote %d rows; want 0", n)
	}
	// The window over (throttle 0), the same reader counts again.
	if n := record("agent-2", 0, "a.md"); n != 1 {
		t.Fatalf("read after the window wrote %d rows; want 1", n)
	}
	if a := get("a.md"); *a.LastReadBy != "agent-2" || a.ReadCount != 3 {
		t.Fatalf("a.md by=%s count=%d; want agent-2, 3", *a.LastReadBy, a.ReadCount)
	}

	// Listings carry the stats too.
	docs, err := r.ListKBDocuments(ctx, sc, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, d := range docs {
		counts[d.Path] = d.ReadCount
	}
	if counts["a.md"] != 3 || counts["b.md"] != 1 {
		t.Fatalf("listed read counts = %v", counts)
	}

	if n := record("", time.Hour, "a.md"); n != 0 {
		t.Errorf("a read with no reader wrote %d rows", n)
	}
	if _, err := r.RecordKBReads(ctx, KBScope{Kind: "global"}, []string{"a.md"}, "x", 0); err == nil {
		t.Error("global scope: want an error")
	}
}
