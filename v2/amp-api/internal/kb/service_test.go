package kb

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/typesense/typesense-go/v4/typesense/api"
)

// Project KBs keep the names and document ids they had before scopes
// existed, so existing knowledge is found without a migration; team and org
// KBs with the same number never share a project's collection or ids.
func TestScopeNaming(t *testing.T) {
	legacyID := func(projectID int, path string, chunk int) string {
		h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%d", projectID, path, chunk)))
		return fmt.Sprintf("%x", h[:8])
	}
	if got := collectionName(Project(5)); got != "kb_5" {
		t.Errorf("project collection = %q, want kb_5", got)
	}
	if got := docID(Project(5), "a.md", 2); got != legacyID(5, "a.md", 2) {
		t.Errorf("project doc id changed: %q", got)
	}

	cols := map[string]Scope{}
	ids := map[string]Scope{}
	for _, sc := range []Scope{Project(5), Team(5), Org(5)} {
		if other, dup := cols[collectionName(sc)]; dup {
			t.Errorf("%s and %s share collection %q", sc, other, collectionName(sc))
		}
		cols[collectionName(sc)] = sc
		if other, dup := ids[docID(sc, "a.md", 0)]; dup {
			t.Errorf("%s and %s share doc ids", sc, other)
		}
		ids[docID(sc, "a.md", 0)] = sc
	}
	if collectionName(Team(5)) != "kb_team_5" || collectionName(Org(5)) != "kb_org_5" {
		t.Errorf("team/org collections = %q, %q", collectionName(Team(5)), collectionName(Org(5)))
	}
}

// Tasks queued before scopes existed carry only a project id.
func TestPayloadScope(t *testing.T) {
	if got := payloadScope("", 0, 7); got != Project(7) {
		t.Errorf("legacy payload scope = %v", got)
	}
	if got := payloadScope(ScopeTeam, 3, 0); got != Team(3) {
		t.Errorf("team payload scope = %v", got)
	}
}

// A reindex that rebuilds the collection (to add embeddings) drops every
// doc first; annotations must come back with the docs. Needs Typesense.
func TestReindexRebuildKeepsAnnotations(t *testing.T) {
	url := os.Getenv("TEST_TYPESENSE_URL")
	if url == "" {
		t.Skip("set TEST_TYPESENSE_URL to run against Typesense")
	}
	key := os.Getenv("TEST_TYPESENSE_KEY")
	if key == "" {
		key = "test"
	}
	s := New(url, key, "", "")
	ctx := context.Background()
	sc := Team(int(time.Now().UnixNano() % 1_000_000_000))
	t.Cleanup(func() { s.Drop(context.Background(), sc) }) //nolint:errcheck

	if _, err := s.WriteDoc(ctx, sc, "a.md", "A", "Alpha doc.", "me", []string{"x"}, "shared by the team", nil); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first note", "second note"} {
		if _, err := s.AnnotateDoc(ctx, sc, "a.md", text, "me"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.reindex(ctx, sc, true); err != nil {
		t.Fatal(err)
	}
	doc, err := s.GetDoc(ctx, sc, "a.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Annotations) != 2 || doc.Annotations[1].Text != "second note" || doc.Reason != "shared by the team" {
		t.Fatalf("after a rebuilding reindex: %+v", doc)
	}
}

// Search's score must be a 0-1 relevance that follows Typesense's order, never
// the raw ~1e18 text_match (#1077).
func TestScoreHit(t *testing.T) {
	f32 := func(f float32) *float32 { return &f }
	i64 := func(i int64) *int64 { return &i }
	fused := func(f float32) *api.SearchResultHitHybridSearchInfo {
		return &api.SearchResultHitHybridSearchInfo{RankFusionScore: &f}
	}

	tests := []struct {
		name        string
		hit         api.SearchResultHit
		rank        int
		wantScore   float64
		wantVector  float64 // -1 means nil
		wantKeyword bool
	}{
		{
			name:        "hybrid hit with both components uses the fusion score",
			hit:         api.SearchResultHit{TextMatch: i64(3465581326462093425), VectorDistance: f32(0.414), HybridSearchInfo: fused(0.75)},
			wantScore:   0.75,
			wantVector:  0.586,
			wantKeyword: true,
		},
		{
			name:        "hybrid keyword-only hit no longer leaks text_match",
			hit:         api.SearchResultHit{TextMatch: i64(3465581326462093425), HybridSearchInfo: fused(0.70)},
			wantScore:   0.70,
			wantVector:  -1,
			wantKeyword: true,
		},
		{
			name:       "hybrid vector-only hit uses the fusion score, not the similarity",
			hit:        api.SearchResultHit{TextMatch: i64(0), VectorDistance: f32(0.325), HybridSearchInfo: fused(0.30)},
			rank:       1,
			wantScore:  0.30,
			wantVector: 0.675,
		},
		{
			name:        "keyword-only collection scores by reciprocal rank",
			hit:         api.SearchResultHit{TextMatch: i64(4620772360776059000)},
			rank:        2,
			wantScore:   1.0 / 3,
			wantVector:  -1,
			wantKeyword: true,
		},
		{
			name:       "vector-only hit without fusion info uses its similarity",
			hit:        api.SearchResultHit{VectorDistance: f32(0.4)},
			wantScore:  0.6,
			wantVector: 0.6,
		},
		{
			name:       "similarity is clamped to 0 for cosine distance above 1",
			hit:        api.SearchResultHit{VectorDistance: f32(1.3)},
			wantScore:  0,
			wantVector: 0,
		},
	}
	const eps = 1e-6
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scoreHit(tt.hit, tt.rank)
			if got.score < tt.wantScore-eps || got.score > tt.wantScore+eps {
				t.Errorf("score = %v, want %v", got.score, tt.wantScore)
			}
			if got.score < 0 || got.score > 1 {
				t.Errorf("score %v outside [0,1]", got.score)
			}
			switch {
			case tt.wantVector < 0 && got.vector != nil:
				t.Errorf("vector = %v, want nil", *got.vector)
			case tt.wantVector >= 0 && got.vector == nil:
				t.Errorf("vector = nil, want %v", tt.wantVector)
			case tt.wantVector >= 0 && (*got.vector < tt.wantVector-eps || *got.vector > tt.wantVector+eps):
				t.Errorf("vector = %v, want %v", *got.vector, tt.wantVector)
			}
			if got.keyword != tt.wantKeyword {
				t.Errorf("keyword = %v, want %v", got.keyword, tt.wantKeyword)
			}
		})
	}
}

// A keyword-only result page scores 1, 1/2, 1/3, ...: never above 1 and
// non-increasing in result order.
func TestScoreHitKeywordOnlyMonotonic(t *testing.T) {
	tm := []int64{4620772360776059000, 3465581326462093425, 2886860116362004500, 2886860116362004500}
	prev := 1.0
	for rank, v := range tm {
		v := v
		got := scoreHit(api.SearchResultHit{TextMatch: &v}, rank).score
		if got > 1 || got > prev {
			t.Fatalf("rank %d: score %v (prev %v) not within (0,1] and non-increasing", rank, got, prev)
		}
		prev = got
	}
}
