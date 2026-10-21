package kb

// adoptEditEntry saves an old pod's edit in two transactions: the
// annotation union, then the content. If the second fails, the first must
// not leave the entry looking merely stale, or the retry re-indexes it from
// the row over what only it held (PR #67 review S4, AMP #2039). These need
// Postgres and Typesense; see store_test.go for the environment.

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// failOnce is a Store whose first UpsertKBDocument (upsert) or
// UpdateKBAnnotations (!upsert) fails without writing, as a DB error or a
// ctx cancelled at shutdown would; later calls pass through.
type failOnce struct {
	Store
	upsert bool
	failed atomic.Bool
}

var errInjected = errors.New("injected store failure")

func (f *failOnce) UpsertKBDocument(ctx context.Context, sc repository.KBScope, w repository.KBDocWrite) (*repository.KBDocument, bool, error) {
	if f.upsert && f.failed.CompareAndSwap(false, true) {
		return nil, false, errInjected
	}
	return f.Store.UpsertKBDocument(ctx, sc, w)
}

func (f *failOnce) UpdateKBAnnotations(ctx context.Context, sc repository.KBScope, path string,
	fn func([]domain.KBAnnotation) ([]domain.KBAnnotation, error)) (*repository.KBDocument, error) {
	if !f.upsert && f.failed.CompareAndSwap(false, true) {
		return nil, errInjected
	}
	return f.Store.UpdateKBAnnotations(ctx, sc, path, fn)
}

// adoptFailures are the two writes adoptEditEntry makes, each failed once.
// After the failed attempt the row has the note merged only if the
// annotation write ran (it runs first); its content is unchanged either way.
var adoptFailures = []struct {
	name       string
	upsert     bool
	noteMerged bool
}{
	{"content write fails", true, true},
	{"annotation merge fails", false, false},
}

// oldPodEditWithNote writes doc.md through s (one resolved annotation), then
// has an old pod edit it in Typesense with new content and a new note. It
// returns the row before the edit and the annotations the row should end up
// with.
func oldPodEditWithNote(t *testing.T, s *Service, repo *repository.Repo, sc Scope, content string) (*repository.KBDocument, []Annotation) {
	t.Helper()
	annotatedDoc(t, s, repo, sc, "doc.md", "original content")
	// Date the row an hour back, so the old pod's edit is in the past too:
	// a content write then dates the row (NOW()) after the entry, as it
	// would in production, and an entry left behind looks merely stale.
	if _, err := rawDB(t).Exec(context.Background(), `UPDATE kb_documents SET updated_at = updated_at - interval '1 hour'
		WHERE scope_kind = $1 AND scope_id = $2 AND path = 'doc.md'`, sc.Kind, sc.ID); err != nil {
		t.Fatal(err)
	}
	row := mustRow(t, repo, sc, "doc.md")
	added := Annotation{Author: "old@x", Text: "note only the old pod has", CreatedAt: row.Annotations[0].CreatedAt + 1}
	oldPodWrite(t, s, sc, indexedDoc{path: "doc.md", title: "Doc v2", content: content,
		author: "old@x", reason: "why", tags: []string{"a"}, annotations: append(slices.Clone(row.Annotations), added),
		updatedAt: row.UpdatedAt.Unix() + 2})
	return row, []Annotation{row.Annotations[0], added}
}

// assertEntryUntouched checks the index still holds the old pod's entry:
// its content, both annotations, and no kb_version (so it was not re-indexed
// from the row).
func assertEntryUntouched(t *testing.T, s *Service, sc Scope, content string, anns []Annotation) {
	t.Helper()
	d, err := s.getIndexed(context.Background(), sc, "doc.md")
	if err != nil {
		t.Fatalf("entry gone: %v", err)
	}
	if d.Content != content || !reflect.DeepEqual(d.Annotations, anns) {
		t.Errorf("entry = %q %+v, want the old pod's %q with %+v", d.Content, d.Annotations, content, anns)
	}
	if v := indexedVersion(t, s, sc, "doc.md"); v != 0 {
		t.Errorf("entry kb_version = %d, want 0 (re-indexed from the row over the old pod's edit)", v)
	}
}

// The reconciler fails one of the two writes that save an old pod's edit
// and note. The row's content is unchanged, the entry is left alone (still
// newer than the row, never re-indexed over), and the next pass saves
// whatever is missing, so both the edit and the note survive.
func TestReconcileAdoptEditRetriesAfterWriteFails(t *testing.T) {
	for _, tc := range adoptFailures {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, sc := backfilledKB(t)
			const content = "wombatlantern edited by an old pod"
			row, wantAnns := oldPodEditWithNote(t, s, repo, sc, content)

			r, store := reconcilerStore(t, repo)
			store.allowEdits = true
			r.SetStore(&failOnce{Store: store, upsert: tc.upsert})

			st := mustReconcile(t, r, sc)
			if st.Failed != 1 || st.AdoptedEdits != 0 || st.Stale != 0 {
				t.Fatalf("first pass = %+v, want 1 failed and nothing re-indexed", st)
			}
			assertAfterFailedAdopt(t, r, repo, sc, row, content, wantAnns, tc.noteMerged)

			st = mustReconcile(t, r, sc)
			if st.AdoptedEdits != 1 || st.Failed != 0 || st.Stale != 0 {
				t.Fatalf("second pass = %+v, want 1 adopted edit", st)
			}
			got := mustRow(t, repo, sc, "doc.md")
			if got.Version != 2 || got.Content != content || got.Title != "Doc v2" || !reflect.DeepEqual(got.Annotations, wantAnns) {
				t.Errorf("row = v%d %q %q %+v, want the old pod's edit at v2 with both annotations", got.Version, got.Title, got.Content, got.Annotations)
			}
			if u, a := store.upserts.Load(), store.annUpdates.Load(); u != 1 || a != 1 {
				t.Errorf("upserts = %d, annotation updates = %d, want 1 and 1 (each saved once)", u, a)
			}
			assertServes(t, r, sc, "doc.md", content, wantAnns, 2)

			if st := mustReconcile(t, r, sc); st.AdoptedEdits != 0 || st.Fixed() != 0 || st.Failed != 0 {
				t.Errorf("third pass = %+v, want nothing to do", st)
			}
		})
	}
}

// assertAfterFailedAdopt checks the state after an adopt attempt that failed
// one write: the row's version, content and updated_at unchanged, the note
// merged only if noteMerged, and the old pod's entry untouched in the index.
func assertAfterFailedAdopt(t *testing.T, s *Service, repo *repository.Repo, sc Scope, row *repository.KBDocument,
	content string, wantAnns []Annotation, noteMerged bool) {
	t.Helper()
	got := mustRow(t, repo, sc, "doc.md")
	if got.Version != row.Version || got.Content != row.Content || !got.UpdatedAt.Equal(row.UpdatedAt) {
		t.Errorf("after the failed attempt row = v%d %q %v, want v%d unchanged", got.Version, got.Content, got.UpdatedAt, row.Version)
	}
	want := row.Annotations
	if noteMerged {
		want = wantAnns
	}
	if !reflect.DeepEqual(got.Annotations, want) {
		t.Errorf("after the failed attempt row annotations = %+v, want %+v", got.Annotations, want)
	}
	assertEntryUntouched(t, s, sc, content, wantAnns)
}

// adoptEditEntry is also the reindex's (coverOldIndex): a reindex that
// fails one of the two writes is refused with the old collection kept, and
// the next reindex adopts the edit and the note before deleting it.
func TestReindexAdoptEditRetriesAfterWriteFails(t *testing.T) {
	for _, tc := range adoptFailures {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, sc := backfilledKB(t)
			ctx := context.Background()
			const content = "quokkaharbor edited by an old pod"
			row, wantAnns := oldPodEditWithNote(t, s, repo, sc, content)

			s.SetStore(&failOnce{Store: repo, upsert: tc.upsert})
			if err := s.Reindex(ctx, sc); !errors.Is(err, errInjected) {
				t.Fatalf("first reindex err = %v, want the injected failure", err)
			}
			assertAfterFailedAdopt(t, s, repo, sc, row, content, wantAnns, tc.noteMerged)

			if err := s.Reindex(ctx, sc); err != nil {
				t.Fatalf("second reindex: %v", err)
			}
			got := mustRow(t, repo, sc, "doc.md")
			if got.Version != 2 || got.Content != content || !reflect.DeepEqual(got.Annotations, wantAnns) {
				t.Errorf("row = v%d %q %+v, want the old pod's edit at v2 with both annotations", got.Version, got.Content, got.Annotations)
			}
			assertServes(t, s, sc, "doc.md", content, wantAnns, 2)
		})
	}
}
