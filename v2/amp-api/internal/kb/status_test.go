package kb

// Tests for KBStatus's durability fields (#1995): stored_docs, indexed_docs
// and backfill. Same setup as store_test.go: TEST_DATABASE_URL and
// TEST_TYPESENSE_URL.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/repository"
)

func mustStatus(t *testing.T, s *Service, sc Scope) *StatusInfo {
	t.Helper()
	st, err := s.KBStatus(context.Background(), sc)
	if err != nil {
		t.Fatalf("KBStatus(%s): %v", sc, err)
	}
	return st
}

func intOf(p *int) string {
	if p == nil {
		return "absent"
	}
	return fmt.Sprint(*p)
}

// stored_docs counts live Postgres rows and indexed_docs the index's own
// chunk-0 count, both exact past the 250-doc page; each moves on its own
// when the other copy is missing a doc.
func TestKBStatusStoredAndIndexedCounts(t *testing.T) {
	s, repo, sc := backfilledKB(t)
	ctx := context.Background()
	const n = 260
	for i := 0; i < n; i++ {
		content := fmt.Sprintf("body %d", i)
		if i == 3 {
			content = longDoc(4, "multichunk") // several chunks; still one doc
		}
		if _, err := s.WriteDoc(ctx, sc, fmt.Sprintf("notes/%03d.md", i), fmt.Sprintf("Note %d", i), content, "me", []string{"t"}, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if c := chunkCount(t, s, sc, "notes/003.md"); c < 3 {
		t.Fatalf("notes/003.md has %d chunks, want several", c)
	}
	st := mustStatus(t, s, sc)
	if st.TotalDocs != n || intOf(st.StoredDocs) != fmt.Sprint(n) || intOf(st.IndexedDocs) != fmt.Sprint(n) ||
		st.IndexError != "" || st.Note != "" || st.DocsByTag["t"] != n {
		t.Fatalf("status = %+v, stored %s, indexed %s; want %d of each", st, intOf(st.StoredDocs), intOf(st.IndexedDocs), n)
	}
	if st.Backfill == nil || st.Backfill.Status != BackfillComplete || st.Backfill.CompletedAt == 0 || st.Backfill.StartedAt == 0 ||
		st.Backfill.DocCount != 0 || st.Backfill.LastError != "" {
		t.Fatalf("backfill = %+v", st.Backfill)
	}

	// A doc written while Typesense is down: stored, not indexed.
	down := New("http://127.0.0.1:1", "x", "", "")
	down.SetStore(repo)
	if _, err := down.WriteDoc(ctx, sc, "unindexed.md", "U", "u", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	// An index entry with no row: indexed, not stored.
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "index-only.md", title: "I", content: "i", updatedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	// A deleted doc leaves both counts.
	if err := s.DeleteDoc(ctx, sc, "notes/000.md", "me"); err != nil {
		t.Fatal(err)
	}
	st = mustStatus(t, s, sc)
	if intOf(st.StoredDocs) != fmt.Sprint(n) || intOf(st.IndexedDocs) != fmt.Sprint(n) || st.TotalDocs != n {
		t.Fatalf("after drift: total %d, stored %s, indexed %s; want %d each", st.TotalDocs, intOf(st.StoredDocs), intOf(st.IndexedDocs), n)
	}
	if _, err := s.ReconcileKB(ctx, sc); err != nil {
		t.Fatal(err)
	}
	// The reconciler indexes the missing doc and copies the index-only entry
	// (no kb_version, dated after the backfill: an old pod's write) into
	// Postgres (#2024), so both counts are n+1.
	if st = mustStatus(t, s, sc); intOf(st.StoredDocs) != fmt.Sprint(n+1) || intOf(st.IndexedDocs) != fmt.Sprint(n+1) {
		t.Fatalf("after reconcile: stored %s, indexed %s; want %d of each", intOf(st.StoredDocs), intOf(st.IndexedDocs), n+1)
	}
}

// With Typesense unreachable, a backfilled KB's status still answers from
// Postgres: indexed_docs is absent and index_error says why.
func TestKBStatusWithIndexDown(t *testing.T) {
	_, repo, sc := backfilledKB(t)
	ctx := context.Background()
	down := New("http://127.0.0.1:1", "x", "", "")
	down.SetStore(repo)
	for _, p := range []string{"a.md", "b.md"} {
		if _, err := down.WriteDoc(ctx, sc, p, p, "body", "me", nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	st := mustStatus(t, down, sc)
	if st.TotalDocs != 2 || intOf(st.StoredDocs) != "2" || st.IndexedDocs != nil || st.IndexError == "" ||
		st.Backfill == nil || st.Backfill.Status != BackfillComplete {
		t.Fatalf("status = %+v, stored %s, indexed %s", st, intOf(st.StoredDocs), intOf(st.IndexedDocs))
	}
}

// The backfill state follows kb_backfills: not_started, failed (with the
// error), started, then complete with the copied doc count.
func TestKBStatusBackfillStates(t *testing.T) {
	s, repo, sc := backfillKB(t) // an index-only KB, as before the deploy
	ctx := context.Background()
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "old.md", title: "Old", content: "old", tags: []string{"old"}, updatedAt: 1_600_000_000}); err != nil {
		t.Fatal(err)
	}

	check := func(stage, status string, stored, total int, lastErr string) *BackfillState {
		t.Helper()
		st := mustStatus(t, s, sc)
		if st.Backfill == nil || st.Backfill.Status != status || st.Backfill.LastError != lastErr ||
			intOf(st.StoredDocs) != fmt.Sprint(stored) || intOf(st.IndexedDocs) != "1" || st.TotalDocs != total {
			t.Fatalf("%s: status = %+v, backfill %+v, stored %s, indexed %s", stage, st, st.Backfill, intOf(st.StoredDocs), intOf(st.IndexedDocs))
		}
		return st.Backfill
	}
	// Reads still merge the index-only doc into total_docs; Postgres has none.
	check("before", BackfillNotStarted, 0, 1, "")
	if err := repo.FailKBBackfill(ctx, storeScope(sc), "count moved"); err != nil {
		t.Fatal(err)
	}
	check("failed", BackfillFailed, 0, 1, "count moved")
	if err := repo.StartKBBackfill(ctx, storeScope(sc)); err != nil {
		t.Fatal(err)
	}
	if b := check("started", BackfillStarted, 0, 1, ""); b.StartedAt == 0 || b.CompletedAt != 0 {
		t.Fatalf("started = %+v", b)
	}
	if _, err := s.BackfillAll(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if b := check("complete", BackfillComplete, 1, 1, ""); b.DocCount != 1 || b.CompletedAt == 0 {
		t.Fatalf("complete = %+v", b)
	}
}

// The global KB has no Postgres copy: stored_docs and backfill are absent
// and the note says why. So is a service with no store. indexed_docs is
// still reported.
func TestKBStatusWithoutStore(t *testing.T) {
	repo := storeTestRepo(t)
	url, key := typesenseEnv(t)
	s := New(url, key, "", "")
	s.SetStore(repo)
	st := mustStatus(t, s, Global())
	if st.StoredDocs != nil || st.Backfill != nil || st.IndexedDocs == nil || *st.IndexedDocs != st.TotalDocs ||
		!strings.Contains(st.Note, "global KB") {
		t.Fatalf("global status = %+v", st)
	}

	plain := New(url, key, "", "")
	sc := Team(1_000_000 + int(time.Now().UnixNano()%1_000_000_000))
	t.Cleanup(func() { plain.dropCollections(context.Background(), collectionName(sc)) }) //nolint:errcheck
	if _, err := plain.WriteDoc(context.Background(), sc, "a.md", "A", "a", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	st = mustStatus(t, plain, sc)
	if st.StoredDocs != nil || st.Backfill != nil || intOf(st.IndexedDocs) != "1" || st.TotalDocs != 1 ||
		!strings.Contains(st.Note, "no Postgres KB store") {
		t.Fatalf("no-store status = %+v, indexed %s", st, intOf(st.IndexedDocs))
	}
}

func TestBackfillStateOf(t *testing.T) {
	started := time.Unix(1_700_000_000, 0)
	done := time.Unix(1_700_000_100, 0)
	tests := []struct {
		name string
		in   *repository.KBBackfill
		want BackfillState
	}{
		{"no row", nil, BackfillState{Status: BackfillNotStarted}},
		{"running", &repository.KBBackfill{StartedAt: started}, BackfillState{Status: BackfillStarted, StartedAt: started.Unix()}},
		{"failed", &repository.KBBackfill{StartedAt: started, LastError: "boom"},
			BackfillState{Status: BackfillFailed, StartedAt: started.Unix(), LastError: "boom"}},
		{"complete", &repository.KBBackfill{StartedAt: started, CompletedAt: &done, DocCount: 7},
			BackfillState{Status: BackfillComplete, StartedAt: started.Unix(), CompletedAt: done.Unix(), DocCount: 7}},
		// A failure recorded after completion doesn't undo it: Postgres
		// already holds every doc.
		{"complete, later error", &repository.KBBackfill{StartedAt: started, CompletedAt: &done, DocCount: 7, LastError: "late"},
			BackfillState{Status: BackfillComplete, StartedAt: started.Unix(), CompletedAt: done.Unix(), DocCount: 7, LastError: "late"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backfillStateOf(tt.in); *got != tt.want {
				t.Errorf("got %+v, want %+v", *got, tt.want)
			}
		})
	}
}

// The JSON keeps every existing field and adds the new ones; absent ones
// are left out rather than sent as null.
func TestStatusInfoJSON(t *testing.T) {
	n, m := 3, 2
	raw, err := json.Marshal(&StatusInfo{TotalDocs: 3, DocsByTag: map[string]int{}, StoredDocs: &n, IndexedDocs: &m,
		Backfill: &BackfillState{Status: BackfillComplete, CompletedAt: 5, DocCount: 3}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"total_docs", "stale_docs", "docs_with_annotations", "total_annotations",
		"unresolved_annotations", "docs_by_tag", "stored_docs", "indexed_docs", "backfill"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %q in %s", k, raw)
		}
	}
	for _, k := range []string{"index_error", "note"} {
		if _, ok := got[k]; ok {
			t.Errorf("unexpected %q in %s", k, raw)
		}
	}
	b := got["backfill"].(map[string]interface{})
	if b["status"] != "complete" || b["doc_count"] != float64(3) || b["completed_at"] != float64(5) {
		t.Errorf("backfill = %v", b)
	}

	raw, _ = json.Marshal(&StatusInfo{DocsByTag: map[string]int{}, Note: "x"})
	if s := string(raw); strings.Contains(s, "stored_docs") || strings.Contains(s, "backfill") || strings.Contains(s, "indexed_docs") {
		t.Errorf("absent fields sent: %s", s)
	}
}
