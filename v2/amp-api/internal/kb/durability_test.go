package kb

// End-to-end durability tests for the Postgres-backed KB (#1998): no doc is
// lost in any of the failure cases below. Each test counts the docs and
// compares a content hash per doc (title, content, tags, author, reason and
// annotations), plus updated_at and the number of index chunks, in Postgres
// and in Typesense.
//
// They need Postgres, Typesense and Valkey (CI provides all three):
//
//	TEST_DATABASE_URL=postgres://... TEST_TYPESENSE_URL=http://localhost:8108 TEST_TYPESENSE_KEY=test \
//	TEST_REDIS_ADDR=localhost:6379 go test ./internal/kb/... -race -count=1 -run Durability
//
// Writes go through the queued path where it matters: a real asynq client
// and worker on Valkey, wired as cmd/server/main.go wires them. Typesense
// failures are injected by flakyTypesense, a reverse proxy in front of the
// test Typesense that can be taken down or made to fail one doc's chunk.
//
// Scenarios and their tests:
//
//  1. Collection deleted entirely, reindex restores it: TestDurabilityCollectionDeleted
//  2. Reindex cancelled mid-way: TestDurabilityReindexCancelled
//  3. Typesense down during writes, reconcile indexes them: TestDurabilityTypesenseDownDuringWrites
//  4. Delete then restore: TestDurabilityDeleteThenRestore
//  5. Backfill of a >250-doc Typesense-only KB: TestDurabilityBackfillLargeKB
//  6. Reindex refuses when Postgres has fewer docs: TestDurabilityReindexRefusesWhenIndexHasMore
//  7. Concurrent writes and annotations, queued: TestDurabilityConcurrentQueuedWrites
//  8. Typesense fails part way through a doc's chunks: TestDurabilityPartialChunkWrite
//  9. Legacy kb:write_doc job queued at deploy: TestDurabilityLegacyQueuedWrite
//
// Rollback (#2027): an old pod's edit or annotation of an existing doc,
// written to Typesense only after a rollback to the old image, is copied
// into the row instead of being overwritten. That is proven in
// reconcile_test.go by TestReconcileAdoptsOldPodEdit (content, title, tags
// and an annotation; previous content kept in kb_document_versions) and
// TestReconcileAdoptsOldPodAnnotation (annotation only), and is not
// repeated here.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/simstech/amp-api/internal/queue"
	"github.com/simstech/amp-api/internal/repository"
)

// ---- Per-doc state: hash, date, chunks ----

// docState is what must survive for one doc.
type docState struct {
	hash      string // sha256 of title, content, tags, author, reason, annotations
	updatedAt int64  // unix seconds
	chunks    int    // index chunks the content makes
}

func contentHash(title, content string, tags []string, author, reason string, anns []Annotation) string {
	if tags == nil {
		tags = []string{}
	}
	b, err := json.Marshal(struct {
		Title, Content, Author, Reason string
		Tags                           []string
		Annotations                    []Annotation
	}{title, content, author, reason, tags, kbAnnotationsOrEmpty(anns)})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func chunksOf(content string) int {
	if n := len(chunkMarkdown(content, 500)); n > 0 {
		return n
	}
	return 1
}

func durableRow(d *repository.KBDocument) docState {
	return docState{
		hash:      contentHash(d.Title, d.Content, d.Tags, d.Author, d.Reason, d.Annotations),
		updatedAt: d.UpdatedAt.Unix(), chunks: chunksOf(d.Content),
	}
}

func durableIndexed(d *Doc) docState {
	return docState{
		hash:      contentHash(d.Title, d.Content, d.Tags, d.Author, d.Reason, d.Annotations),
		updatedAt: d.UpdatedAt, chunks: chunksOf(d.Content),
	}
}

// storedState is every live row of sc.
func storedState(t *testing.T, repo *repository.Repo, sc Scope) map[string]docState {
	t.Helper()
	out := map[string]docState{}
	if err := repo.StreamKBDocuments(context.Background(), storeScope(sc), func(d repository.KBDocument) error {
		out[d.Path] = durableRow(&d)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// assertStored checks Postgres holds exactly want: same docs, same hashes
// and dates.
func assertStored(t *testing.T, repo *repository.Repo, sc Scope, want map[string]docState) {
	t.Helper()
	got := storedState(t, repo, sc)
	if len(got) != len(want) {
		t.Errorf("postgres holds %d live docs, want %d", len(got), len(want))
	}
	if n, err := repo.CountKBDocuments(context.Background(), storeScope(sc)); err != nil || n != len(want) {
		t.Errorf("CountKBDocuments = %d, %v; want %d", n, err, len(want))
	}
	for p, w := range want {
		g, ok := got[p]
		switch {
		case !ok:
			t.Errorf("postgres lost %s", p)
		case g.hash != w.hash:
			t.Errorf("postgres %s: content hash %.12s, want %.12s", p, g.hash, w.hash)
		case g.updatedAt != w.updatedAt:
			t.Errorf("postgres %s: updated_at %d, want %d", p, g.updatedAt, w.updatedAt)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("postgres has an unexpected doc %s", p)
		}
	}
}

// assertIndexed checks the search index holds exactly want: same docs (one
// chunk 0 each), same hashes and dates, and exactly the chunks each doc's
// content makes (no leftover chunks of an older version).
func assertIndexed(t *testing.T, s *Service, sc Scope, want map[string]docState) {
	t.Helper()
	ctx := context.Background()
	if n, err := s.countIndexed(ctx, sc); err != nil || n != len(want) {
		t.Errorf("index holds %d docs (err %v), want %d", n, err, len(want))
	}
	for p, w := range want {
		d, err := s.getIndexed(ctx, sc, p)
		if err != nil {
			t.Errorf("index lost %s: %v", p, err)
			continue
		}
		g := durableIndexed(d)
		if g.hash != w.hash {
			t.Errorf("index %s: content hash %.12s, want %.12s", p, g.hash, w.hash)
		}
		if g.updatedAt != w.updatedAt {
			t.Errorf("index %s: updated_at %d, want %d", p, g.updatedAt, w.updatedAt)
		}
		if c := chunkCount(t, s, sc, p); c != w.chunks {
			t.Errorf("index %s: %d chunks, want %d", p, c, w.chunks)
		}
	}
}

func assertDurable(t *testing.T, s *Service, repo *repository.Repo, sc Scope, want map[string]docState) {
	t.Helper()
	assertStored(t, repo, sc, want)
	assertIndexed(t, s, sc, want)
}

// ---- Typesense with injected failures ----

// flakyTypesense proxies the test Typesense. down makes every request fail
// with 503; failChunk makes upserts of failPath's chunk failChunk fail.
type flakyTypesense struct {
	url       string
	down      atomic.Bool
	mu        sync.Mutex
	failPath  string
	failChunk int
	failed    atomic.Int32 // requests refused by failPath/failChunk
	// onUpsert, if set, sees every document upsert's collection and chunk.
	onUpsert func(col, path string, chunk int)
}

func newFlakyTypesense(t *testing.T) *flakyTypesense {
	t.Helper()
	tsURL, _ := typesenseEnv(t)
	target, err := url.Parse(tsURL)
	if err != nil {
		t.Fatal(err)
	}
	f := &flakyTypesense{failChunk: -1}
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, `{"message":"typesense down (test)"}`, http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/documents") && r.URL.Query().Get("action") == "upsert" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var doc struct {
				Path  string `json:"path"`
				Chunk int    `json:"chunk_index"`
			}
			_ = json.Unmarshal(body, &doc)
			col := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/collections/"), "/documents")
			f.mu.Lock()
			hook, fail := f.onUpsert, f.failPath != "" && doc.Path == f.failPath && doc.Chunk == f.failChunk
			f.mu.Unlock()
			if hook != nil {
				hook(col, doc.Path, doc.Chunk)
			}
			if fail {
				f.failed.Add(1)
				http.Error(w, `{"message":"chunk write failed (test)"}`, http.StatusServiceUnavailable)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *flakyTypesense) failUpsert(path string, chunk int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPath, f.failChunk = path, chunk
}

func (f *flakyTypesense) setOnUpsert(fn func(col, path string, chunk int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onUpsert = fn
}

// ---- The queued path: asynq on Valkey ----

type kbQueue struct {
	t       *testing.T
	opt     asynq.RedisClientOpt
	client  *asynq.Client
	insp    *asynq.Inspector
	srv     *asynq.Server
	handler *Service
	delay   time.Duration
}

// newKBQueue connects s to the test Valkey the way main.go does: s enqueues
// through an asynq client, and (once start runs) a worker with s's handlers
// drains the queue. A failed job is retried after retryDelay. The queue is
// emptied before and deleted after the test.
func newKBQueue(t *testing.T, s *Service, retryDelay time.Duration) *kbQueue {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run the queued KB durability tests against Valkey")
	}
	opt := queue.RedisConnOpt(addr, "")
	q := &kbQueue{t: t, opt: opt, client: asynq.NewClient(opt), insp: asynq.NewInspector(opt), handler: s, delay: retryDelay}
	q.insp.DeleteQueue(queue.QueueName, true) //nolint:errcheck // leftovers of an aborted run; absent is fine
	s.SetQueueClient(q.client)
	t.Cleanup(func() {
		if q.srv != nil {
			q.srv.Shutdown()
		}
		q.insp.DeleteQueue(queue.QueueName, true) //nolint:errcheck
		q.client.Close()
		q.insp.Close()
	})
	return q
}

func (q *kbQueue) start() {
	q.t.Helper()
	delay := q.delay
	q.srv = asynq.NewServer(q.opt, asynq.Config{
		Concurrency:              4,
		Queues:                   map[string]int{queue.QueueName: 1},
		RetryDelayFunc:           func(int, error, *asynq.Task) time.Duration { return delay },
		TaskCheckInterval:        50 * time.Millisecond,
		DelayedTaskCheckInterval: 100 * time.Millisecond,
		ShutdownTimeout:          5 * time.Second,
		LogLevel:                 asynq.FatalLevel,
	})
	mux := asynq.NewServeMux()
	mux.HandleFunc(queue.TypeKBWriteDoc, q.handler.HandleWriteDocTask)
	mux.HandleFunc(queue.TypeKBDeleteDoc, q.handler.HandleDeleteDocTask)
	mux.HandleFunc(queue.TypeKBIndexDoc, q.handler.HandleIndexDocTask)
	if err := q.srv.Start(mux); err != nil {
		q.t.Fatal(err)
	}
}

func (q *kbQueue) info() *asynq.QueueInfo {
	q.t.Helper()
	qi, err := q.insp.GetQueueInfo(queue.QueueName)
	if err != nil {
		if errors.Is(err, asynq.ErrQueueNotFound) {
			return &asynq.QueueInfo{}
		}
		q.t.Fatal(err)
	}
	return qi
}

// waitFor polls the queue until cond holds, failing the test after 60s.
func (q *kbQueue) waitFor(what string, cond func(*asynq.QueueInfo) bool) {
	q.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		qi := q.info()
		if cond(qi) {
			return
		}
		if time.Now().After(deadline) {
			q.t.Fatalf("queue never %s: pending %d active %d scheduled %d retry %d archived %d",
				what, qi.Pending, qi.Active, qi.Scheduled, qi.Retry, qi.Archived)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// drain waits until every job has run successfully.
func (q *kbQueue) drain() {
	q.t.Helper()
	q.waitFor("drained", func(qi *asynq.QueueInfo) bool {
		return qi.Pending+qi.Active+qi.Scheduled+qi.Retry == 0
	})
	if qi := q.info(); qi.Archived != 0 {
		q.t.Fatalf("%d jobs archived (failed for good)", qi.Archived)
	}
}

// ---- Services ----

// durableKB is a service on Typesense at tsURL (the test Typesense or a
// flakyTypesense) backed by the test Postgres, for scope sc. The KB's
// collections are dropped after the test through the real Typesense.
func durableKB(t *testing.T, sc Scope, tsURL string) (*Service, *repository.Repo) {
	t.Helper()
	repo := storeTestRepo(t)
	realURL, key := typesenseEnv(t)
	if tsURL == "" {
		tsURL = realURL
	}
	s := New(tsURL, key, "", "")
	s.SetStore(repo)
	cleanup := New(realURL, key, "", "")
	cleanup.SetStore(repo)
	t.Cleanup(func() { cleanup.Drop(context.Background(), sc) }) //nolint:errcheck
	return s, repo
}

func randomTeam() Scope    { return Team(1_000_000 + rand.IntN(1_000_000_000)) }
func randomProject() Scope { return Project(1_000_000 + rand.IntN(1_000_000_000)) }

// seedDocs writes n docs: doc 0 spans several chunks, every third doc has
// an annotation (every sixth one resolved).
func seedDocs(t *testing.T, s *Service, sc Scope, n int, word string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("docs/%03d.md", i)
		content := fmt.Sprintf("%s doc %d body", word, i)
		if i == 0 {
			content = longDoc(4, word)
		}
		if _, err := s.WriteDoc(ctx, sc, p, fmt.Sprintf("Doc %d", i), content, fmt.Sprintf("a%d@x", i%3),
			[]string{"seed", fmt.Sprintf("g%d", i%4)}, fmt.Sprintf("reason %d", i%2), nil); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if _, err := s.AnnotateDoc(ctx, sc, p, fmt.Sprintf("note on %d", i), "rev@x"); err != nil {
				t.Fatal(err)
			}
			if i%6 == 0 {
				if _, err := s.ResolveAnnotation(ctx, sc, p, 0, true, "res@x"); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// ---- 1. Typesense collection deleted entirely ----

// Every collection of a KB (the alias, its target and any build) is
// deleted from Typesense. Postgres is untouched, and a reindex restores
// every doc with identical content, annotations and updated_at.
func TestDurabilityCollectionDeleted(t *testing.T) {
	sc := randomTeam()
	s, repo := durableKB(t, sc, "")
	q := newKBQueue(t, s, 100*time.Millisecond)
	q.start()
	ctx := context.Background()
	seedDocs(t, s, sc, 12, "walrus")
	q.drain()
	want := storedState(t, repo, sc)
	if len(want) != 12 {
		t.Fatalf("seeded %d docs, want 12", len(want))
	}
	assertDurable(t, s, repo, sc, want)
	// Once through an alias, so the wipe takes an alias, its target and the
	// real collection it replaced.
	if err := s.Reindex(ctx, sc); err != nil {
		t.Fatal(err)
	}

	if err := s.dropCollections(ctx, collectionName(sc)); err != nil {
		t.Fatal(err)
	}
	if kc := kbCols(t, s, sc); kc.real || kc.alias != "" || len(kc.builds) != 0 {
		t.Fatalf("after deleting the collection: %+v, want nothing left in Typesense", kc)
	}
	if n := mustCount(t, s, sc); n != 0 {
		t.Fatalf("index still holds %d docs", n)
	}
	assertStored(t, repo, sc, want)

	time.Sleep(1100 * time.Millisecond) // a reindex that re-dated docs would show
	if err := s.Reindex(ctx, sc); err != nil {
		t.Fatalf("reindex after the collection was deleted: %v", err)
	}
	assertDurable(t, s, repo, sc, want)
	if got := searchPaths(t, s, sc, "walrus"); len(got) == 0 {
		t.Error("search finds nothing after the reindex")
	}
}

// ---- 2. Reindex cancelled mid-way ----

// A reindex cancelled while it is writing the new build (part way through
// the docs), and one cancelled just before the swap, both leave the old
// collection serving every doc unchanged and drop their build. A retry then
// completes.
func TestDurabilityReindexCancelled(t *testing.T) {
	ts := newFlakyTypesense(t)
	sc := randomTeam()
	s, repo := durableKB(t, sc, ts.url)
	q := newKBQueue(t, s, 100*time.Millisecond)
	q.start()
	bg := context.Background()
	seedDocs(t, s, sc, 30, "heron")
	q.drain()
	want := storedState(t, repo, sc)
	assertDurable(t, s, repo, sc, want)

	// Cancel when the 10th doc upsert lands in the new build.
	ctx, cancel := context.WithCancel(bg)
	var intoBuild atomic.Int32
	base := collectionName(sc)
	ts.setOnUpsert(func(col, _ string, _ int) {
		if isBuildOf(base, col) && intoBuild.Add(1) == 10 {
			cancel()
		}
	})
	err := s.Reindex(ctx, sc)
	cancel()
	ts.setOnUpsert(nil)
	if err == nil {
		t.Fatal("a reindex cancelled mid-build returned nil")
	}
	if intoBuild.Load() < 10 {
		t.Fatalf("only %d upserts reached the build; the cancel never fired mid-way", intoBuild.Load())
	}
	if kc := kbCols(t, s, sc); !kc.real || kc.alias != "" || len(kc.builds) != 0 {
		t.Fatalf("after a cancel mid-build: %+v, want the original collection only", kc)
	}
	assertDurable(t, s, repo, sc, want)

	// The retry completes.
	if err := s.Reindex(bg, sc); err != nil {
		t.Fatalf("retry: %v", err)
	}
	serving := kbCols(t, s, sc).alias
	if !isBuildOf(base, serving) {
		t.Fatalf("after the retry no alias serves %s", base)
	}
	assertDurable(t, s, repo, sc, want)

	// Cancelled just before the swap, once an alias serves.
	ctx, cancel = context.WithCancel(bg)
	s.reindexHook = func(_ context.Context, stage string) {
		if stage == "verified" {
			cancel()
		}
	}
	err = s.Reindex(ctx, sc)
	cancel()
	s.reindexHook = nil
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reindex cancelled before the swap returned %v, want context.Canceled", err)
	}
	if kc := kbCols(t, s, sc); kc.alias != serving || !slices.Equal(kc.builds, []string{serving}) {
		t.Fatalf("after a cancel before the swap: %+v, want alias still -> %s and no other build", kc, serving)
	}
	assertDurable(t, s, repo, sc, want)
	if err := s.Reindex(bg, sc); err != nil {
		t.Fatalf("second retry: %v", err)
	}
	assertDurable(t, s, repo, sc, want)
}

// ---- 3. Typesense unreachable during writes ----

// With Typesense down, new docs, a rewrite, an annotation and a resolve all
// succeed into Postgres and read back from it; their queued index jobs
// fail. Once Typesense is back, one reconcile indexes every one of them and
// leaves Postgres as it was.
func TestDurabilityTypesenseDownDuringWrites(t *testing.T) {
	ts := newFlakyTypesense(t)
	sc := randomTeam()
	s, repo := durableKB(t, sc, ts.url)
	q := newKBQueue(t, s, time.Hour) // failed jobs wait: the reconcile, not a retry, must fix the index
	q.start()
	ctx := context.Background()
	seedDocs(t, s, sc, 4, "otter")
	q.drain()
	before := storedState(t, repo, sc)
	assertIndexed(t, s, sc, before)

	ts.down.Store(true)
	writes := []func() error{
		func() error {
			_, err := s.WriteDoc(ctx, sc, "new/one.md", "One", "otter written while typesense was down", "me", []string{"down"}, "why", nil)
			return err
		},
		func() error {
			_, err := s.WriteDoc(ctx, sc, "new/long.md", "Long", longDoc(3, "otter"), "me", nil, "", nil)
			return err
		},
		func() error {
			_, err := s.WriteDoc(ctx, sc, "docs/001.md", "Doc 1 v2", "otter rewritten while down", "me", []string{"seed"}, "", nil)
			return err
		},
		func() error { _, err := s.AnnotateDoc(ctx, sc, "docs/002.md", "noted while down", "me"); return err },
		func() error { _, err := s.ResolveAnnotation(ctx, sc, "docs/003.md", 0, true, "me"); return err },
	}
	for i, w := range writes {
		if err := w(); err != nil {
			t.Fatalf("write %d with typesense down: %v", i, err)
		}
	}
	if d, err := s.GetDoc(ctx, sc, "new/one.md"); err != nil || d.Content != "otter written while typesense was down" {
		t.Fatalf("read back with typesense down: %+v, %v", d, err)
	}
	q.waitFor("holding the 5 failed index jobs", func(qi *asynq.QueueInfo) bool {
		return qi.Retry == len(writes) && qi.Pending+qi.Active == 0
	})
	want := storedState(t, repo, sc)
	if len(want) != 6 {
		t.Fatalf("postgres holds %d docs, want 6", len(want))
	}
	for _, p := range []string{"docs/001.md", "docs/002.md", "docs/003.md"} {
		if want[p].hash == before[p].hash {
			t.Fatalf("%s: postgres doesn't have the change made while typesense was down", p)
		}
	}

	ts.down.Store(false)
	// The index is still behind: the new docs are missing and the changed ones stale.
	if n := mustCount(t, s, sc); n != 4 {
		t.Fatalf("index holds %d docs before the reconcile, want the 4 from before", n)
	}
	r := reconciler(t, repo)
	st := mustReconcile(t, r, sc)
	if st.Missing != 2 || st.Stale != 3 || st.Failed != 0 {
		t.Fatalf("reconcile stats = %+v, want 2 missing and 3 stale", st)
	}
	assertDurable(t, s, repo, sc, want)
	if st := mustReconcile(t, r, sc); st.Fixed() != 0 {
		t.Errorf("second reconcile still fixed %+v", st)
	}
}

// ---- 4. Delete then restore ----

// Deleted docs leave the index and the live rows but stay in the trash;
// restoring them brings back the same content, annotations (resolved state
// included) and updated_at, in Postgres and in the index.
func TestDurabilityDeleteThenRestore(t *testing.T) {
	sc := randomTeam()
	s, repo := durableKB(t, sc, "")
	q := newKBQueue(t, s, 100*time.Millisecond)
	q.start()
	ctx := context.Background()
	seedDocs(t, s, sc, 7, "puffin") // 000 (multi-chunk, resolved note), 003 (open note), 006 (resolved note)
	q.drain()
	want := storedState(t, repo, sc)
	assertDurable(t, s, repo, sc, want)

	gone := []string{"docs/000.md", "docs/003.md", "docs/006.md", "docs/004.md"}
	time.Sleep(1100 * time.Millisecond) // a restore that re-dated docs would show
	for _, p := range gone {
		if err := s.DeleteDoc(ctx, sc, p, "deleter@x"); err != nil {
			t.Fatal(err)
		}
	}
	q.drain()
	left := map[string]docState{}
	for p, st := range want {
		if !contains(gone, p) {
			left[p] = st
		}
	}
	assertDurable(t, s, repo, sc, left)
	trash, err := s.ListDeletedDocs(ctx, sc)
	if err != nil || len(trash) != len(gone) {
		t.Fatalf("trash = %+v, %v; want %d docs", trash, err, len(gone))
	}
	for _, p := range searchPaths(t, s, sc, "puffin") {
		if contains(gone, p) {
			t.Errorf("deleted %s still found by search", p)
		}
	}

	for _, p := range gone {
		d, err := s.RestoreDoc(ctx, sc, p, "restorer@x")
		if err != nil {
			t.Fatal(err)
		}
		if durableIndexed(d) != want[p] {
			t.Errorf("RestoreDoc(%s) returned a different doc", p)
		}
	}
	q.drain()
	assertDurable(t, s, repo, sc, want)
	if trash, err := s.ListDeletedDocs(ctx, sc); err != nil || len(trash) != 0 {
		t.Errorf("trash after restore = %+v, %v", trash, err)
	}
	if !contains(searchPaths(t, s, sc, "puffin paragraph"), "docs/000.md") {
		t.Error("restored multi-chunk doc not found by search")
	}
}

// ---- 5. Backfill from a Typesense-only KB of more than 250 docs ----

// An old (Typesense-only) amp-api wrote 263 docs, past the old 250-doc
// page cap, with annotations and a multi-chunk doc. The backfill copies
// every one into Postgres with the same hash and date as the index, and a
// second run changes nothing.
func TestDurabilityBackfillLargeKB(t *testing.T) {
	s, repo, sc := backfillKB(t)
	url, key := typesenseEnv(t)
	legacy := New(url, key, "", "") // no store: the pre-Postgres write path
	ctx := context.Background()
	const n = 263
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("old/%03d.md", i)
		content := fmt.Sprintf("gannet legacy doc %d", i)
		if i == 7 {
			content = longDoc(4, "gannet")
		}
		if _, err := legacy.WriteDoc(ctx, sc, p, fmt.Sprintf("Old %d", i), content, fmt.Sprintf("a%d@x", i%5),
			[]string{"legacy", fmt.Sprintf("g%d", i%3)}, fmt.Sprintf("r%d", i%2), nil); err != nil {
			t.Fatal(err)
		}
		if i%20 == 0 {
			if _, err := legacy.AnnotateDoc(ctx, sc, p, fmt.Sprintf("legacy note %d", i), "rev@x"); err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.AnnotateDoc(ctx, sc, p, "second", "rev2@x"); err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.ResolveAnnotation(ctx, sc, p, 1, true, "res@x"); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := map[string]docState{}
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("old/%03d.md", i)
		d, err := legacy.getIndexed(ctx, sc, p)
		if err != nil {
			t.Fatal(err)
		}
		want[p] = durableIndexed(d)
	}
	if c := mustCount(t, s, sc); c != n {
		t.Fatalf("legacy index holds %d docs, want %d", c, n)
	}
	assertStored(t, repo, sc, map[string]docState{}) // nothing in Postgres yet

	stats, err := s.BackfillAll(ctx, nil)
	if err != nil || len(stats) != 1 || stats[0].Inserted != n || stats[0].Docs != n {
		t.Fatalf("backfill = %+v, %v; want %d inserted", stats, err, n)
	}
	assertDurable(t, s, repo, sc, want)
	if c, err := s.CountDocs(ctx, sc); err != nil || c != n {
		t.Errorf("CountDocs = %d, %v; want %d", c, err, n)
	}

	before := rowsSnapshot(t, sc)
	stats, err = s.BackfillAll(ctx, nil)
	if err != nil || len(stats) != 1 || stats[0].Skipped != "already complete" || stats[0].Inserted != 0 {
		t.Fatalf("second backfill = %+v, %v; want a no-op", stats, err)
	}
	if after := rowsSnapshot(t, sc); !slices.Equal(before, after) {
		t.Error("the second backfill changed postgres rows")
	}
	assertDurable(t, s, repo, sc, want)
}

// ---- 6. Reindex refuses when Postgres has fewer docs than Typesense ----

// The index holds docs Postgres has no row for (an old pod's write, and a
// stray entry with a kb_version). A reindex refuses and changes nothing:
// every index entry, Postgres row and collection is as it was.
func TestDurabilityReindexRefusesWhenIndexHasMore(t *testing.T) {
	sc := randomTeam()
	s, repo := durableKB(t, sc, "")
	q := newKBQueue(t, s, 100*time.Millisecond)
	q.start()
	ctx := context.Background()
	seedDocs(t, s, sc, 5, "ibis")
	q.drain()
	stored := storedState(t, repo, sc)

	extra := []indexedDoc{
		{path: "old-pod.md", title: "Old pod", content: "ibis written by an old pod", author: "old@x", updatedAt: time.Now().Unix()},
		{path: "stray.md", title: "Stray", content: "ibis stray entry", author: "x@x", updatedAt: 1, version: 3},
	}
	indexed := map[string]docState{}
	for p, st := range stored {
		indexed[p] = st
	}
	for _, d := range extra {
		if _, err := s.indexChunks(ctx, sc, d); err != nil {
			t.Fatal(err)
		}
		indexed[d.path] = docState{hash: contentHash(d.title, d.content, d.tags, d.author, d.reason, nil), updatedAt: d.updatedAt, chunks: 1}
	}
	assertIndexed(t, s, sc, indexed)
	colsBefore := kbCols(t, s, sc)
	rowsBefore := rowsSnapshot(t, sc)

	if err := s.Reindex(ctx, sc); !errors.Is(err, ErrReindexRefused) {
		t.Fatalf("reindex returned %v, want ErrReindexRefused", err)
	}
	if kc := kbCols(t, s, sc); fmt.Sprint(kc) != fmt.Sprint(colsBefore) {
		t.Fatalf("collections changed: %+v -> %+v", colsBefore, kc)
	}
	assertStored(t, repo, sc, stored)
	assertIndexed(t, s, sc, indexed)
	if !slices.Equal(rowsBefore, rowsSnapshot(t, sc)) {
		t.Error("a refused reindex changed postgres rows")
	}
}

// ---- 7. Concurrent writes and an annotation, queued ----

// For several docs at once, two writes to the same path and an annotation
// race through the queued path (4 workers, index jobs in any order). Each
// doc ends with the content of the last committed write, both annotations,
// version 3, and an index entry identical to its row.
func TestDurabilityConcurrentQueuedWrites(t *testing.T) {
	sc := randomTeam()
	s, repo := durableKB(t, sc, "")
	q := newKBQueue(t, s, 100*time.Millisecond)
	q.start()
	ctx := context.Background()
	paths := []string{"race/a.md", "race/b.md", "race/c.md", "race/d.md", "race/e.md"}
	for _, p := range paths {
		if _, err := s.WriteDoc(ctx, sc, p, "Race", "kestrel v1 of "+p, "me", []string{"race"}, "", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AnnotateDoc(ctx, sc, p, "first note", "first@x"); err != nil {
			t.Fatal(err)
		}
	}
	q.drain()

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 3*len(paths))
	for _, p := range paths {
		for _, op := range []func() error{
			func() error {
				_, err := s.WriteDoc(ctx, sc, p, "Race", "kestrel v2 of "+p, "w2@x", []string{"race"}, "", nil)
				return err
			},
			func() error {
				_, err := s.WriteDoc(ctx, sc, p, "Race", longDoc(3, "kestrel")+"v3 of "+p, "w3@x", []string{"race"}, "", nil)
				return err
			},
			func() error { _, err := s.AnnotateDoc(ctx, sc, p, "second note", "second@x"); return err },
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := op(); err != nil {
					errs <- fmt.Errorf("%s: %w", p, err)
				}
			}()
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	q.drain()

	want := map[string]docState{}
	for _, p := range paths {
		row := mustRow(t, repo, sc, p)
		vs := versionRows(t, sc, p)
		if row.Version != 3 || len(vs) == 0 {
			t.Fatalf("%s: version %d, history %d rows; want version 3", p, row.Version, len(vs))
		}
		last := vs[len(vs)-1]
		if !strings.HasPrefix(last, "3|") || !strings.HasSuffix(last, "|"+row.Content) {
			t.Errorf("%s: row content is not the last committed write (history ends %.40q)", p, last)
		}
		if row.Content != "kestrel v2 of "+p && row.Content != longDoc(3, "kestrel")+"v3 of "+p {
			t.Errorf("%s: content %.40q is neither write", p, row.Content)
		}
		var notes []string
		for _, a := range row.Annotations {
			notes = append(notes, a.Author+":"+a.Text)
		}
		sort.Strings(notes)
		if !slices.Equal(notes, []string{"first@x:first note", "second@x:second note"}) {
			t.Errorf("%s: annotations %v, want both", p, notes)
		}
		want[p] = durableRow(row)
	}
	assertDurable(t, s, repo, sc, want)
}

// ---- 8. Typesense fails part way through writing a doc's chunks ----

// A rewrite's index job fails on one of the doc's chunks. The Postgres row
// holds the new version intact, and search keeps finding the doc (old or
// new chunks, never neither). The failed job's retry (queued path), or the
// next reconcile when the failure hit chunk 0, converges the index to the
// new version with no chunk of the old one left.
//
// Limit (not covered by the reconciler): when chunk 0 was written and a
// later chunk failed, chunk 0 already carries the new kb_version and
// updated_at, so the reconciler sees the entry as current and the old
// version's later chunks stay until the job's retry, the doc's next index
// or a reindex. Only the retry case is tested for that.
func TestDurabilityPartialChunkWrite(t *testing.T) {
	ctx := context.Background()
	v1, v2 := longDoc(6, "albatross"), longDoc(4, "cormorant")
	if chunksOf(v1) <= chunksOf(v2) || chunksOf(v2) < 3 {
		t.Fatalf("v1 has %d chunks, v2 %d; want v1 longer and v2 at least 3", chunksOf(v1), chunksOf(v2))
	}
	// neverNeither checks the doc stays readable and searchable from the
	// index while the failure lasts.
	neverNeither := func(t *testing.T, s *Service, sc Scope, p string) {
		t.Helper()
		d, err := s.getIndexed(ctx, sc, p)
		if err != nil {
			t.Fatalf("index lost %s while its rewrite was failing: %v", p, err)
		}
		if d.Content != v1 && d.Content != v2 {
			t.Errorf("index holds neither version of %s", p)
		}
		if !contains(searchPaths(t, s, sc, "albatross"), p) && !contains(searchPaths(t, s, sc, "cormorant"), p) {
			t.Errorf("search finds neither version of %s", p)
		}
	}

	t.Run("later chunk fails, queued retry converges", func(t *testing.T) {
		ts := newFlakyTypesense(t)
		sc := randomTeam()
		s, repo := durableKB(t, sc, ts.url)
		q := newKBQueue(t, s, 200*time.Millisecond)
		q.start()
		if _, err := s.WriteDoc(ctx, sc, "big.md", "Big", v1, "me", []string{"x"}, "", nil); err != nil {
			t.Fatal(err)
		}
		seedDocs(t, s, sc, 3, "tern")
		q.drain()
		assertDurable(t, s, repo, sc, storedState(t, repo, sc))

		ts.failUpsert("big.md", 2) // chunks 0 and 1 land, chunk 2 fails
		if _, err := s.WriteDoc(ctx, sc, "big.md", "Big", v2, "me", []string{"x"}, "", nil); err != nil {
			t.Fatalf("write with a failing index: %v", err)
		}
		q.waitFor("retrying the failed index job", func(qi *asynq.QueueInfo) bool { return ts.failed.Load() > 0 && qi.Retry+qi.Scheduled > 0 })
		want := storedState(t, repo, sc)
		if row := mustRow(t, repo, sc, "big.md"); row.Content != v2 || row.Version != 2 {
			t.Fatalf("postgres row = version %d, %d bytes; want v2 intact", row.Version, len(row.Content))
		}
		neverNeither(t, s, sc, "big.md")
		if c := chunkCount(t, s, sc, "big.md"); c != chunksOf(v1) {
			t.Fatalf("mid-failure the doc has %d chunks, want %d (new 0-1, old 2..)", c, chunksOf(v1))
		}

		ts.failUpsert("", -1)
		q.drain()
		assertDurable(t, s, repo, sc, want)
		if got := searchPaths(t, s, sc, "albatross"); len(got) != 0 {
			t.Errorf("old version's chunks still found by search: %v", got)
		}
	})

	t.Run("chunk 0 fails, reconcile converges", func(t *testing.T) {
		ts := newFlakyTypesense(t)
		sc := randomTeam()
		s, repo := durableKB(t, sc, ts.url) // no queue: the failure is only logged, as when a job is lost
		if _, err := s.WriteDoc(ctx, sc, "big.md", "Big", v1, "me", []string{"x"}, "", nil); err != nil {
			t.Fatal(err)
		}
		seedDocs(t, s, sc, 3, "tern")
		assertDurable(t, s, repo, sc, storedState(t, repo, sc))

		ts.failUpsert("big.md", 0)
		if _, err := s.WriteDoc(ctx, sc, "big.md", "Big", v2, "me", []string{"x"}, "", nil); err != nil {
			t.Fatalf("write with a failing index: %v", err)
		}
		if ts.failed.Load() == 0 {
			t.Fatal("the injected failure never fired")
		}
		want := storedState(t, repo, sc)
		if row := mustRow(t, repo, sc, "big.md"); row.Content != v2 || row.Version != 2 {
			t.Fatalf("postgres row = version %d; want v2 intact", row.Version)
		}
		neverNeither(t, s, sc, "big.md")

		ts.failUpsert("", -1)
		st := mustReconcile(t, reconciler(t, repo), sc)
		if st.Stale != 1 || st.Failed != 0 {
			t.Fatalf("reconcile stats = %+v, want 1 stale", st)
		}
		assertDurable(t, s, repo, sc, want)
		if got := searchPaths(t, s, sc, "albatross"); len(got) != 0 {
			t.Errorf("old version's chunks still found by search: %v", got)
		}
	})
}

// ---- 9. Legacy kb:write_doc job queued at deploy ----

// Jobs an old amp-api queued before the deploy (kb:write_doc in its
// pre-scope payload format: project_id only, the content inline) are still
// in Valkey when a new pod's worker starts. Each is saved to Postgres
// before it is indexed: even with Typesense down when the jobs first run,
// the rows hold their content; the retries then index them. A job for a new
// path inserts it, one for a live doc applies the write (the overwritten
// content stays in the version history, annotations kept), and one for a
// doc deleted since stays deleted.
func TestDurabilityLegacyQueuedWrite(t *testing.T) {
	ts := newFlakyTypesense(t)
	sc := randomProject()
	s, repo := durableKB(t, sc, ts.url)
	q := newKBQueue(t, s, 200*time.Millisecond)
	ctx := context.Background()

	// Before the deploy: the KB exists and is backfilled, with a live doc
	// and a deleted one (written by the new code synchronously here).
	s.SetQueueClient(nil)
	if _, err := s.WriteDoc(ctx, sc, "live.md", "Live", "pelican before the deploy", "me", []string{"x"}, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnnotateDoc(ctx, sc, "live.md", "kept note", "rev@x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteDoc(ctx, sc, "gone.md", "Gone", "pelican deleted", "me", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDoc(ctx, sc, "gone.md", "me"); err != nil {
		t.Fatal(err)
	}
	if done, err := repo.IsKBBackfillComplete(ctx, storeScope(sc)); err != nil || !done {
		t.Fatalf("KB not backfilled: %v, %v", done, err)
	}
	s.SetQueueClient(q.client)

	// Queued by the old amp-api, in its own payload format.
	legacy := func(path, title, content string) {
		b, err := json.Marshal(map[string]any{
			"project_id": sc.ID, "path": path, "title": title, "content": content, "author": "old-pod@x", "tags": []string{"legacy"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.client.EnqueueContext(ctx, asynq.NewTask(queue.TypeKBWriteDoc, b), asynq.Queue(queue.QueueName)); err != nil {
			t.Fatal(err)
		}
	}
	newContent := longDoc(3, "pelican") + "queued for a new path"
	legacy("new.md", "New", newContent)
	legacy("live.md", "Live", "pelican queued edit of the live doc")
	legacy("gone.md", "Gone", "pelican queued edit of the deleted doc")

	// The new pod starts while Typesense is unreachable.
	ts.down.Store(true)
	q.start()
	q.waitFor("retrying the legacy jobs", func(qi *asynq.QueueInfo) bool { return qi.Pending+qi.Active == 0 && qi.Retry+qi.Scheduled == 3 })
	stored := storedState(t, repo, sc)
	if len(stored) != 2 {
		t.Fatalf("postgres holds %d live docs, want new.md and live.md", len(stored))
	}
	if row := mustRow(t, repo, sc, "new.md"); row.Content != newContent || row.Author != "old-pod@x" {
		t.Fatalf("new.md not saved before indexing: %+v", row)
	}
	if row := mustRow(t, repo, sc, "live.md"); row.Content != "pelican queued edit of the live doc" || len(row.Annotations) != 1 {
		t.Fatalf("live.md not saved before indexing: %+v", row)
	}

	ts.down.Store(false)
	q.drain()
	want := storedState(t, repo, sc)
	if want["new.md"] != stored["new.md"] || want["live.md"] != stored["live.md"] {
		t.Error("a retried legacy job changed the saved doc")
	}
	assertDurable(t, s, repo, sc, want)
	if row := mustRow(t, repo, sc, "live.md"); row.Annotations[0].Text != "kept note" {
		t.Errorf("live.md lost its annotation: %+v", row.Annotations)
	}
	hist := versionRows(t, sc, "live.md")
	if len(hist) < 2 || !strings.HasSuffix(hist[0], "|pelican before the deploy") {
		t.Errorf("live.md history %v, want the pre-deploy content kept as version 1", hist)
	}
	if del, exists := deletedRow(t, rawDB(t), sc, "gone.md"); !del || !exists {
		t.Errorf("gone.md: deleted=%v exists=%v, want it still deleted", del, exists)
	}
	if _, err := s.getIndexed(ctx, sc, "gone.md"); err == nil {
		t.Error("gone.md is back in the index")
	}
}
