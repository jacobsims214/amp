package kb

// PR #67 review S2 (#2037): a KB must not be left complete with 0 docs, its
// docs hidden, because its collection could not be seen at that moment:
// Typesense still loading after a restart (GET /health not ok), or the
// collection briefly unlisted. The backfill and the lazy completion wait for
// a healthy Typesense, and a KB completed with 0 docs is copied again,
// insert-only, by the next run once its collection is back. Needs Postgres
// and Typesense; see store_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/simstech/amp-api/internal/repository"
)

// hidingTypesense proxies the test Typesense. While hide is set it acts as
// if the base collection name (and its builds and alias) did not exist: it
// leaves them out of the collection and alias lists and answers 404 for
// them. While unhealthy is set, GET /health answers 503 {"ok":false}, as a
// Typesense still loading its data does. Everything else passes through.
type hidingTypesense struct {
	url       string
	base      string
	hide      atomic.Bool
	unhealthy atomic.Bool
}

func newHidingTypesense(t *testing.T, base string) *hidingTypesense {
	t.Helper()
	tsURL, _ := typesenseEnv(t)
	target, err := url.Parse(tsURL)
	if err != nil {
		t.Fatal(err)
	}
	h := &hidingTypesense{base: base}
	hidden := func(name string) bool { return name == h.base || isBuildOf(h.base, name) }
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && h.unhealthy.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ok":false}`))
			return
		}
		if !h.hide.Load() {
			proxy.ServeHTTP(w, r)
			return
		}
		for _, prefix := range []string{"/collections/", "/aliases/"} {
			if name, ok := strings.CutPrefix(r.URL.Path, prefix); ok {
				name, _, _ = strings.Cut(name, "/")
				if hidden(name) {
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
					return
				}
			}
		}
		if r.Method != http.MethodGet || (r.URL.Path != "/collections" && r.URL.Path != "/aliases") {
			proxy.ServeHTTP(w, r)
			return
		}
		// A list: fetch it uncompressed and drop the hidden names.
		r.Header.Del("Accept-Encoding")
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		if rec.Code == http.StatusOK {
			body = filterHidden(t, r.URL.Path, body, hidden)
		}
		for k, v := range rec.Header() {
			if k != "Content-Length" {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = io.Copy(w, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return h
}

// filterHidden drops hidden names from a GET /collections or GET /aliases
// response body.
func filterHidden(t *testing.T, path string, body []byte, hidden func(string) bool) []byte {
	keep := func(items []map[string]any) []map[string]any {
		out := items[:0]
		for _, it := range items {
			if n, _ := it["name"].(string); !hidden(n) {
				out = append(out, it)
			}
		}
		return out
	}
	var out []byte
	var err error
	if path == "/collections" {
		var cols []map[string]any
		if err = json.Unmarshal(body, &cols); err == nil {
			out, err = json.Marshal(keep(cols))
		}
	} else {
		var al struct {
			Aliases []map[string]any `json:"aliases"`
		}
		if err = json.Unmarshal(body, &al); err == nil {
			al.Aliases = keep(al.Aliases)
			out, err = json.Marshal(al)
		}
	}
	if err != nil {
		t.Errorf("filter %s: %v", path, err)
		return body
	}
	return out
}

// hiddenKB returns a stored service talking to Typesense through a
// hidingTypesense, and a fresh orphan project KB holding pre-Postgres docs
// in its index only (no kb_version, original dates, annotations, one
// multi-chunk doc), plus one entry indexed from a row since purged
// (kb_version set, no row). want is the pre-Postgres docs.
func hiddenKB(t *testing.T) (*Service, *repository.Repo, Scope, *hidingTypesense, map[string]indexedDoc) {
	t.Helper()
	repo := storeTestRepo(t)
	_, key := typesenseEnv(t)
	sc := Project(1_000_000 + rand.IntN(1_000_000_000))
	h := newHidingTypesense(t, collectionName(sc))
	s := New(h.url, key, "", "")
	s.SetStore(repo)
	s.backfillOnly = func(x Scope) bool { return x == sc }
	t.Cleanup(func() { s.dropCollections(context.Background(), collectionName(sc)) }) //nolint:errcheck

	ctx := context.Background()
	base := int64(1_600_000_000)
	want := map[string]indexedDoc{}
	for i := 0; i < 12; i++ {
		d := indexedDoc{
			path: fmt.Sprintf("old/%02d.md", i), title: fmt.Sprintf("Old %d", i), content: fmt.Sprintf("only in the index %d", i),
			author: "a@x", reason: "r", tags: []string{"t"}, updatedAt: base + int64(i)*3600,
		}
		if i%4 == 0 {
			d.annotations = []Annotation{{Author: "rev", Text: fmt.Sprintf("note %d", i), CreatedAt: base + 5}}
		}
		if i == 3 {
			d.content = longDoc(4, "multichunk")
		}
		if _, err := s.indexChunks(ctx, sc, d); err != nil {
			t.Fatal(err)
		}
		want[d.path] = d
	}
	if _, err := s.indexChunks(ctx, sc, indexedDoc{path: "purged.md", title: "P", content: "had a row", updatedAt: base, version: 4}); err != nil {
		t.Fatal(err)
	}
	return s, repo, sc, h, want
}

// assertAllCopied checks every doc of want has a row with its content,
// metadata, annotations and original updated_at, and purged.md none.
func assertAllCopied(t *testing.T, repo *repository.Repo, sc Scope, want map[string]indexedDoc) {
	t.Helper()
	for path, w := range want {
		row := mustRow(t, repo, sc, path)
		if row.Title != w.title || row.Content != w.content || row.Author != w.author || row.Reason != w.reason ||
			fmt.Sprint(row.Tags) != fmt.Sprint(w.tags) {
			t.Errorf("%s: row = %+v", path, row)
		}
		if row.UpdatedAt.Unix() != w.updatedAt {
			t.Errorf("%s: updated_at %v, want %d", path, row.UpdatedAt, w.updatedAt)
		}
		if fmt.Sprintf("%+v", row.Annotations) != fmt.Sprintf("%+v", kbAnnotationsOrEmpty(w.annotations)) {
			t.Errorf("%s: annotations %+v, want %+v", path, row.Annotations, w.annotations)
		}
	}
	if c, err := repo.CountKBDocuments(context.Background(), storeScope(sc)); err != nil || c != len(want) {
		t.Errorf("postgres holds %d docs, %v; want %d", c, err, len(want))
	}
	if _, err := repo.GetKBDocument(context.Background(), storeScope(sc), "purged.md"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("purged.md (kb_version set, no row) was copied: %v", err)
	}
}

// A KB whose collection can't be seen during BackfillAll is not left as an
// empty completion hiding its docs: every doc is in Postgres once the
// collection is back and the backfill runs again.
func TestBackfillRecoversKBHiddenDuringRun(t *testing.T) {
	fastBackfillRetries(t)
	ctx := context.Background()

	t.Run("typesense healthy, collection unlisted", func(t *testing.T) {
		s, repo, sc, h, want := hiddenKB(t)
		owners := func(context.Context) ([]repository.KBScope, error) { return []repository.KBScope{storeScope(sc)}, nil }
		h.hide.Store(true)
		// Typesense says it is healthy and lists no collection for the KB:
		// it is completed with 0 docs (the case a wrong but healthy
		// TYPESENSE_URL gives).
		stats, err := s.BackfillAll(ctx, owners)
		if err != nil || len(stats) != 1 || stats[0].Docs != 0 || len(stats[0].Collections) != 0 {
			t.Fatalf("hidden run = %+v, %v", stats, err)
		}
		if b := backfillState(t, repo, sc); b.CompletedAt == nil || b.DocCount != 0 {
			t.Fatalf("kb_backfills after the hidden run = %+v", b)
		}

		// The collection is back: the next run re-checks the 0-doc
		// completion and copies every doc, insert-only.
		h.hide.Store(false)
		stats, err = s.BackfillAll(ctx, owners)
		if err != nil || len(stats) != 1 {
			t.Fatalf("second run = %+v, %v", stats, err)
		}
		st := stats[0]
		if !st.Rechecked || st.Skipped != "" || st.Inserted != len(want) || st.Present != 0 || st.FromStore != 1 || st.Docs != len(want)+1 {
			t.Fatalf("second run stats = %+v; want rechecked, %d inserted, 1 indexed from postgres", st, len(want))
		}
		assertAllCopied(t, repo, sc, want)
		b := backfillState(t, repo, sc)
		if b.CompletedAt == nil || b.DocCount != len(want)+1 || b.LastError != "" {
			t.Fatalf("kb_backfills after the re-check = %+v", b)
		}
		// Reads (Postgres only, the KB is complete) see the docs.
		if c, err := s.CountDocs(ctx, sc); err != nil || c != len(want) {
			t.Errorf("CountDocs = %d, %v; want %d", c, err, len(want))
		}

		// A third run: the KB now has docs recorded, so it is skipped and
		// nothing changes.
		before := snapshotRows(t, repo, sc)
		stats, err = s.BackfillAll(ctx, owners)
		if err != nil || len(stats) != 1 || stats[0].Skipped != "already complete" || stats[0].Inserted != 0 {
			t.Fatalf("third run = %+v, %v", stats, err)
		}
		if after := snapshotRows(t, repo, sc); fmt.Sprint(before) != fmt.Sprint(after) {
			t.Error("the third run changed postgres rows")
		}
		if b2 := backfillState(t, repo, sc); !b2.CompletedAt.Equal(*b.CompletedAt) {
			t.Errorf("the third run re-recorded completion: %v then %v", b.CompletedAt, b2.CompletedAt)
		}
	})

	t.Run("typesense not healthy", func(t *testing.T) {
		s, repo, sc, h, want := hiddenKB(t)
		owners := func(context.Context) ([]repository.KBScope, error) { return []repository.KBScope{storeScope(sc)}, nil }
		// Still loading: /health not ok and the collection not listed yet.
		h.hide.Store(true)
		h.unhealthy.Store(true)
		stats, err := s.BackfillAll(ctx, owners)
		if err != nil || len(stats) != 1 {
			t.Fatalf("unhealthy run = %+v, %v", stats, err)
		}
		b := backfillState(t, repo, sc)
		if b.CompletedAt != nil || !strings.Contains(b.LastError, "not healthy") {
			t.Fatalf("kb_backfills after the unhealthy run = %+v; want failed, not complete", b)
		}
		// Nor does a read complete it lazily meanwhile.
		if done, err := s.backfillDone(ctx, sc); err != nil || done {
			t.Fatalf("backfillDone while unhealthy = %v, %v; want false", done, err)
		}

		// Loaded: the next run copies everything.
		h.hide.Store(false)
		h.unhealthy.Store(false)
		stats, err = s.BackfillAll(ctx, owners)
		if err != nil || len(stats) != 1 || stats[0].Rechecked || stats[0].Inserted != len(want)+1 {
			// First backfill: purged.md is copied too (a first backfill
			// copies every entry, as before #2037).
			t.Fatalf("healthy run = %+v, %v", stats, err)
		}
		if b := backfillState(t, repo, sc); b.CompletedAt == nil || b.DocCount != len(want)+1 {
			t.Fatalf("kb_backfills after the healthy run = %+v", b)
		}
		for path, w := range want {
			if row := mustRow(t, repo, sc, path); row.Content != w.content || row.UpdatedAt.Unix() != w.updatedAt {
				t.Errorf("%s: row = %+v", path, row)
			}
		}
	})
}

// backfillDone does not mark a KB complete while Typesense's /health is not
// ok, even when no collection is listed for it; once it is ok, a KB with no
// collection completes lazily as before.
func TestBackfillDoneWaitsForHealthyTypesense(t *testing.T) {
	repo := storeTestRepo(t)
	_, key := typesenseEnv(t)
	ctx := context.Background()
	sc := Team(1_000_000 + rand.IntN(1_000_000_000))
	h := newHidingTypesense(t, collectionName(sc))
	s := New(h.url, key, "", "")
	s.SetStore(repo)
	t.Cleanup(func() { s.dropCollections(context.Background(), collectionName(sc)) }) //nolint:errcheck

	h.unhealthy.Store(true)
	if done, err := s.backfillDone(ctx, sc); err != nil || done {
		t.Fatalf("backfillDone while unhealthy = %v, %v; want false", done, err)
	}
	if done, _ := repo.IsKBBackfillComplete(ctx, storeScope(sc)); done {
		t.Fatal("KB marked complete while typesense was not healthy")
	}
	// A read meanwhile does not complete it either.
	if _, err := s.ListDocs(ctx, sc, ""); err != nil {
		t.Logf("ListDocs while unhealthy: %v", err)
	}
	if done, _ := repo.IsKBBackfillComplete(ctx, storeScope(sc)); done {
		t.Fatal("a read marked the KB complete while typesense was not healthy")
	}

	h.unhealthy.Store(false)
	if done, err := s.backfillDone(ctx, sc); err != nil || !done {
		t.Fatalf("backfillDone once healthy = %v, %v; want true", done, err)
	}
	if b, err := repo.GetKBBackfill(ctx, storeScope(sc)); err != nil || b.CompletedAt == nil || b.DocCount != 0 {
		t.Fatalf("kb_backfills = %+v, %v", b, err)
	}
}
