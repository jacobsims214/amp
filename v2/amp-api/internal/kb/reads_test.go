package kb

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"
)

// RecordReads (#159) works on the Postgres store alone (no index needed),
// dedupes paths, and the doc read back and listed carries created_at and the
// read stats. The global KB records nothing.
func TestRecordReadsShowsOnDocAndListing(t *testing.T) {
	repo := storeTestRepo(t)
	s := New("http://127.0.0.1:1", "x", "", "")
	s.SetStore(repo)
	sc := Team(1_000_000 + rand.IntN(1_000_000_000))
	ctx := context.Background()
	if err := repo.CompleteKBBackfill(ctx, storeScope(sc), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteDoc(ctx, sc, "read.md", "Read", "body", "me", nil, "why", nil); err != nil {
		t.Fatal(err)
	}

	doc, err := s.GetDoc(ctx, sc, "read.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.CreatedAt == 0 || doc.LastReadAt != 0 || doc.LastReadBy != "" || doc.ReadCount != 0 {
		t.Fatalf("unread doc stats = %+v", doc.ReadStats)
	}

	if err := s.RecordReads(ctx, sc, []string{"read.md", "read.md"}, "agent@x"); err != nil {
		t.Fatal(err)
	}
	doc, err = s.GetDoc(ctx, sc, "read.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.LastReadBy != "agent@x" || doc.ReadCount != 1 || time.Since(time.Unix(doc.LastReadAt, 0)) > time.Minute {
		t.Fatalf("after one (deduped) read: %+v", doc.ReadStats)
	}
	// A repeat read inside ReadThrottle isn't counted.
	if err := s.RecordReads(ctx, sc, []string{"read.md"}, "agent@x"); err != nil {
		t.Fatal(err)
	}
	docs, _, err := s.ListDocsPage(ctx, sc, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].ReadCount != 1 || docs[0].LastReadBy != "agent@x" || docs[0].CreatedAt == 0 {
		t.Fatalf("listing = %+v", docs)
	}

	if err := s.RecordReads(ctx, Global(), []string{"concepts.md"}, "agent@x"); err != nil {
		t.Errorf("global KB: %v; want a no-op", err)
	}
}
