package kb

import (
	"crypto/sha256"
	"fmt"
	"testing"
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
