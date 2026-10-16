package kb

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"
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
