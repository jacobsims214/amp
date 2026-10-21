package kb

// The backfill copies every doc that the Typesense index holds for a
// project, team or org KB into Postgres, once, so that Postgres really is
// the source of truth (decisions/kb-postgres-source-of-truth.md). Before
// this change Typesense was the only copy of those docs.
//
// It is insert-only: a path that already has a row, live or soft-deleted,
// is left exactly as it is (InsertKBDocumentIfMissing), so it never
// overwrites a newer write and never revives a deleted doc. It never deletes
// anything, in Postgres or in Typesense.
//
// A KB is recorded complete (CompleteKBBackfill) only when every chunk-0
// doc of its collection was exported (Typesense's export endpoint: the
// whole collection, no 250-doc page cap), the number exported equals the
// collection's chunk-0 count before and after the copy, and Postgres then
// has a row for every exported path. Anything else records the KB failed
// (FailKBBackfill) and leaves it incomplete: reads keep merging the index,
// reindex and the reconciler keep refusing it, and the next startup retries.
//
// Reads, reindex and the reconciler depend on completion, so a KB with no
// search collection at all (a new project, team or org, or one whose index
// is gone) has nothing to copy and is marked complete with 0 docs, both here
// and lazily the first time the service touches it (backfillDone). That is
// only done while Typesense reports itself healthy (GET /health ok): one
// still loading after a restart can list no collections for a while, and
// completing then would hide every doc it holds. A KB completed with 0 docs
// stays re-checkable: each run copies it again, insert-only, if it has a
// collection or alias by then (recheckEmptyKB).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/typesense/typesense-go/v4/typesense/api"
	"github.com/typesense/typesense-go/v4/typesense/api/pointer"

	"github.com/simstech/amp-api/internal/repository"
)

const (
	// backfillLockKey is the cross-replica lock a backfill run holds.
	backfillLockKey = "kb_backfill"
	// backfillBy is changed_by on the version row a backfilled doc starts with.
	backfillBy = "kb-backfill"
	// backfillAttempts is how often one KB is tried in a run when its
	// collection changes during the copy (a write landing mid-backfill).
	backfillAttempts = 5
)

// Waiting for a KB's reindex lock (held by a Drop or a reconcile): poll
// every backfillLockPoll, for at most backfillLockWait. Variables for tests.
var (
	backfillLockPoll  = 500 * time.Millisecond
	backfillLockWait  = 2 * time.Minute
	backfillRetryWait = time.Second
)

// ErrBackfillPending is returned (wrapped with ErrReindexRefused) by a
// reindex of a stored KB whose backfill has not completed: the index may
// still hold the only copy of some docs.
var ErrBackfillPending = errors.New("the KB's backfill from its search index has not completed")

// errBackfillUnstable means the collection changed while it was being
// copied (its chunk-0 count moved); the copy is retried.
var errBackfillUnstable = errors.New("the collection changed during the backfill")

// errTypesenseUnhealthy means Typesense's GET /health did not answer ok (it
// is down, or still loading its data after a restart). A KB is never marked
// complete with nothing copied while it says so: an empty collection list
// may only mean the collections have not loaded yet.
var errTypesenseUnhealthy = errors.New("typesense is not healthy (GET /health not ok)")

// typesenseHealthTimeout bounds one health check.
var typesenseHealthTimeout = 5 * time.Second

// KBOwnerLister lists the KBs Postgres knows an owner for (every project,
// archived ones included, team and org): repository.Repo.ListKBOwnerScopes.
type KBOwnerLister func(ctx context.Context) ([]repository.KBScope, error)

// BackfillCollectionStats is what the backfill found in one collection.
type BackfillCollectionStats struct {
	Collection string
	Found      int // chunk-0 docs exported
	Inserted   int // copied into Postgres
	Present    int // already had a row (live or deleted), left alone
}

// BackfillStats is one KB's backfill.
type BackfillStats struct {
	Scope       Scope
	Collections []BackfillCollectionStats
	Docs        int // distinct paths found across the collections (doc_count)
	Inserted    int
	Present     int
	// FromStore counts, on a re-check of a KB completed with 0 docs, the
	// paths whose entry carries kb_version: it was indexed from a Postgres
	// row, so it is not copied (a path with no row now was purged).
	FromStore int
	// Rechecked is set when this was a re-check of a KB already completed
	// with 0 docs (recheckEmptyKB), not its first backfill.
	Rechecked bool
	// Skipped says why nothing was done ("" = the KB was backfilled now).
	Skipped string
}

// RunBackfill backfills every KB that is not yet complete, once, under the
// cross-replica lock (e.g. repository.TryNamedLock): a replica that can't
// take it leaves the run to the one that has it. It logs and returns; it
// never fails startup. Run it in the background.
func (s *Service) RunBackfill(ctx context.Context, owners KBOwnerLister, lock ReindexLocker) {
	if s.store == nil {
		return
	}
	if lock != nil {
		release, ok, err := lock(ctx, backfillLockKey)
		if err != nil {
			s.log.Warn("kb backfill: could not take the backfill lock; the next startup retries", "err", err)
			return
		}
		if !ok {
			s.log.Info("kb backfill: another replica is running it")
			return
		}
		defer release()
	}
	if _, err := s.BackfillAll(ctx, owners); err != nil {
		s.log.Error("kb backfill run failed; the next startup retries", "err", err)
	}
}

// BackfillAll backfills every KB that has a collection in Typesense (found
// by listing Typesense itself, so archived projects and KBs whose owner is
// gone are included) and marks the KBs owners lists that have no collection
// at all complete (only while Typesense is healthy). A KB found in Typesense
// that was completed earlier with 0 docs is re-checked (recheckEmptyKB). A
// KB that fails is recorded failed and logged, and the run carries on. The
// error is only for failing to list Typesense, or ctx ending.
func (s *Service) BackfillAll(ctx context.Context, owners KBOwnerLister) ([]BackfillStats, error) {
	if s.store == nil {
		return nil, nil
	}
	found, err := s.discoverKBScopes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list typesense collections: %w", err)
	}
	all := map[Scope]bool{}
	inTypesense := map[Scope]bool{}
	for _, sc := range found {
		all[sc] = true
		inTypesense[sc] = true
	}
	if owners != nil {
		rs, err := owners(ctx)
		if err != nil {
			// Only KBs without a collection come from here; those are also
			// marked lazily on first use.
			s.log.Warn("kb backfill: could not list projects, teams and orgs; backfilling the collections found", "err", err)
		}
		for _, r := range rs {
			all[Scope{Kind: r.Kind, ID: r.ID}] = true
		}
	}
	scopes := make([]Scope, 0, len(all))
	for sc := range all {
		if s.backfillOnly != nil && !s.backfillOnly(sc) {
			continue
		}
		scopes = append(scopes, sc)
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Kind != scopes[j].Kind {
			return scopes[i].Kind < scopes[j].Kind
		}
		return scopes[i].ID < scopes[j].ID
	})

	out := make([]BackfillStats, 0, len(scopes))
	var done, failed, skipped, rechecked, inserted int
	for _, sc := range scopes {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		// Only a KB with something in Typesense can need its 0-doc
		// completion re-checked; the rest are skipped without asking.
		st, err := s.backfillKB0(ctx, sc, inTypesense[sc])
		switch {
		case err != nil:
			failed++
		case st.Skipped != "":
			skipped++
		case st.Rechecked:
			rechecked++
		default:
			done++
		}
		inserted += st.Inserted
		out = append(out, st)
	}
	s.log.Info("kb backfill run done", "kbs", len(scopes), "completed", done, "failed", failed,
		"already_complete", skipped, "rechecked_empty", rechecked, "docs_inserted", inserted)
	return out, nil
}

// BackfillKB copies one KB's indexed docs into Postgres and records it
// complete, or records it failed and returns the error. A KB already
// complete is left alone (Skipped set), unless it was completed with 0 docs:
// that is re-checked (recheckEmptyKB).
func (s *Service) BackfillKB(ctx context.Context, sc Scope) (BackfillStats, error) {
	return s.backfillKB0(ctx, sc, true)
}

// backfillKB0 is BackfillKB; recheck says whether a KB completed with 0
// docs is re-checked (BackfillAll passes false for KBs it found nothing of
// in Typesense, which have nothing to re-check).
func (s *Service) backfillKB0(ctx context.Context, sc Scope, recheck bool) (BackfillStats, error) {
	st := BackfillStats{Scope: sc}
	if !s.stored(sc) {
		return st, fmt.Errorf("backfill %s: not a stored KB", sc)
	}
	rs := storeScope(sc)
	b, err := s.store.GetKBBackfill(ctx, rs)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		// never started
	case err != nil:
		return st, fmt.Errorf("backfill %s: check completion: %w", sc, err)
	case b.CompletedAt != nil && b.DocCount == 0 && recheck:
		return s.recheckEmptyKB(ctx, sc)
	case b.CompletedAt != nil:
		st.Skipped = "already complete"
		return st, nil
	}
	st, err = s.backfillKB(ctx, sc)
	if err != nil {
		s.log.Error("kb backfill failed; the KB stays incomplete and the next startup retries", "kb", sc.String(), "err", err)
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if ferr := s.store.FailKBBackfill(fctx, rs, err.Error()); ferr != nil {
			s.log.Error("kb backfill: could not record the failure", "kb", sc.String(), "err", ferr)
		}
		return st, err
	}
	return st, nil
}

func (s *Service) backfillKB(ctx context.Context, sc Scope) (BackfillStats, error) {
	st := BackfillStats{Scope: sc}
	rs := storeScope(sc)
	// Hold the KB's reindex lock, so a Drop can't remove the collection
	// half way through the copy and the reconciler leaves it alone.
	release, err := s.waitReindexLock(ctx, sc)
	if err != nil {
		return st, err
	}
	defer release()
	if err := s.store.StartKBBackfill(ctx, rs); err != nil {
		return st, fmt.Errorf("record the start: %w", err)
	}
	// Ask before listing: a Typesense still loading can list no
	// collections, and the KB would be completed with nothing copied.
	if err := s.typesenseHealthy(ctx); err != nil {
		return st, err
	}
	st, err = s.backfillCopy(ctx, sc, false)
	if err != nil {
		return st, err
	}
	if len(st.Collections) == 0 {
		// Nothing listed: confirm Typesense was healthy throughout, so an
		// instance that went down or started reloading mid-way doesn't
		// complete the KB empty.
		if err := s.typesenseHealthy(ctx); err != nil {
			return st, err
		}
	}
	if err := s.store.CompleteKBBackfill(ctx, rs, st.Docs); err != nil {
		return st, fmt.Errorf("record completion: %w", err)
	}
	if len(st.Collections) == 0 {
		s.log.Info("kb backfill: no search collection, nothing to copy; marked complete", "kb", sc.String())
	} else {
		s.log.Info("kb backfill complete", "kb", sc.String(), "docs", st.Docs, "inserted", st.Inserted, "already_present", st.Present)
	}
	return st, nil
}

// backfillCopy runs backfillOnce, again while the collection changes under
// it (errBackfillUnstable), up to backfillAttempts times.
func (s *Service) backfillCopy(ctx context.Context, sc Scope, recheck bool) (BackfillStats, error) {
	for attempt := 1; ; attempt++ {
		st, err := s.backfillOnce(ctx, sc, recheck)
		if err == nil {
			return st, nil
		}
		if !errors.Is(err, errBackfillUnstable) || attempt >= backfillAttempts {
			return st, err
		}
		s.log.Info("kb backfill: the collection changed during the copy; trying again", "kb", sc.String(), "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(backfillRetryWait):
		}
	}
}

// recheckEmptyKB re-runs the insert-only copy for a KB already completed
// with 0 docs, in case it was completed while its collection could not be
// seen (Typesense still loading, TYPESENSE_URL pointing at the wrong
// instance, a collection briefly unreadable) and the index still holds its
// only copy of some docs. Entries that carry kb_version are not copied: they
// were indexed from a Postgres row, so a path with no row now was purged and
// must not come back. Inserts are insert-only (InsertKBDocumentIfMissing),
// so a path with any row is left exactly as it is.
//
// started_at and the failure state are left alone: the KB stays complete
// whatever happens, and a failed re-check is logged and returned (the next
// run tries again). When something was found, completion is recorded again
// with the number of distinct paths in the index, so the KB is not
// re-checked after that; a KB that still has nothing indexed is Skipped.
func (s *Service) recheckEmptyKB(ctx context.Context, sc Scope) (BackfillStats, error) {
	st := BackfillStats{Scope: sc, Rechecked: true}
	release, err := s.waitReindexLock(ctx, sc)
	if err != nil {
		return st, fmt.Errorf("re-check %s: %w", sc, err)
	}
	defer release()
	st, err = s.backfillCopy(ctx, sc, true)
	st.Rechecked = true
	if err != nil {
		s.log.Warn("kb backfill: re-check of a KB completed with 0 docs failed; it stays complete and the next run retries",
			"kb", sc.String(), "err", err)
		return st, fmt.Errorf("re-check %s: %w", sc, err)
	}
	if st.Docs == 0 {
		st.Skipped = "already complete"
		return st, nil
	}
	if err := s.store.CompleteKBBackfill(ctx, storeScope(sc), st.Docs); err != nil {
		return st, fmt.Errorf("re-check %s: record completion: %w", sc, err)
	}
	if st.Inserted > 0 {
		s.log.Warn("kb backfill: a KB completed with 0 docs had docs only in its search index; copied them into postgres",
			"kb", sc.String(), "docs", st.Docs, "inserted", st.Inserted, "already_present", st.Present, "indexed_from_postgres", st.FromStore)
	} else {
		s.log.Info("kb backfill: re-checked a KB completed with 0 docs; nothing was missing from postgres",
			"kb", sc.String(), "docs", st.Docs, "already_present", st.Present, "indexed_from_postgres", st.FromStore)
	}
	return st, nil
}

// typesenseHealthy returns nil when Typesense's GET /health answers ok, and
// errTypesenseUnhealthy (wrapping the cause) when it does not or can't be
// reached.
func (s *Service) typesenseHealthy(ctx context.Context) error {
	ok, err := s.client.Health(ctx, typesenseHealthTimeout)
	if err != nil {
		return fmt.Errorf("%w: %v", errTypesenseUnhealthy, err)
	}
	if !ok {
		return errTypesenseUnhealthy
	}
	return nil
}

// waitReindexLock takes sc's reindex lock, waiting while a Drop or a
// reconcile holds it.
func (s *Service) waitReindexLock(ctx context.Context, sc Scope) (func(), error) {
	deadline := time.Now().Add(backfillLockWait)
	for {
		release, err := s.beginReindex(ctx, sc)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, ErrReindexRunning) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the KB's reindex lock stayed taken for %s: %w", backfillLockWait, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backfillLockPoll):
		}
	}
}

// backfillSources lists the collections a KB's docs can be read from: the
// real collection named by its base name (not yet migrated to an alias) and
// the alias's target. Builds that no alias points at are left out: a
// reindex built them from Postgres rows, so they hold nothing Postgres
// lacks, and an old one could hold a doc purged since.
func (s *Service) backfillSources(ctx context.Context, sc Scope) (sources []string, kc kbCollections, err error) {
	kc, err = s.kbCollections(ctx, collectionName(sc))
	if err != nil {
		return nil, kc, err
	}
	if kc.real {
		sources = append(sources, kc.base)
	}
	if kc.alias != "" {
		if _, err := s.client.Collection(kc.alias).Retrieve(ctx); err == nil {
			sources = append(sources, kc.alias)
		} else if !isNotFound(err) {
			return nil, kc, fmt.Errorf("read the alias target %s: %w", kc.alias, err)
		} else {
			s.log.Warn("kb backfill: the KB's alias points at a missing collection", "kb", sc.String(), "alias", kc.base, "target", kc.alias)
		}
	}
	for _, b := range kc.builds {
		if b != kc.alias {
			s.log.Info("kb backfill: ignoring a reindex build no alias points at", "kb", sc.String(), "collection", b)
		}
	}
	return sources, kc, nil
}

// backfillOnce exports each source collection, inserts every doc Postgres
// has no row for, and checks the result. With recheck (recheckEmptyKB), a
// path whose newest entry carries kb_version is counted in FromStore and
// neither copied nor required to have a row.
func (s *Service) backfillOnce(ctx context.Context, sc Scope, recheck bool) (BackfillStats, error) {
	st := BackfillStats{Scope: sc}
	rs := storeScope(sc)
	sources, _, err := s.backfillSources(ctx, sc)
	if err != nil {
		return st, err
	}
	type pick struct {
		doc     *Doc
		version int
		src     int
	}
	merged := map[string]pick{}
	counts := make([]int, len(sources))
	for i, col := range sources {
		cs := BackfillCollectionStats{Collection: col}
		docs, versions, err := s.exportChunk0Versions(ctx, col)
		if err != nil {
			return st, fmt.Errorf("export %s: %w", col, err)
		}
		n, err := s.countIn(ctx, col)
		if err != nil {
			return st, fmt.Errorf("count %s: %w", col, err)
		}
		paths := map[string]bool{}
		for _, d := range docs {
			paths[d.Path] = true
		}
		if n != len(docs) || len(paths) != len(docs) {
			return st, fmt.Errorf("%w: %s exported %d chunk-0 docs (%d distinct paths) but counts %d",
				errBackfillUnstable, col, len(docs), len(paths), n)
		}
		counts[i] = n
		cs.Found = n
		st.Collections = append(st.Collections, cs)
		for j, d := range docs {
			// A path in two collections (a collection shadowing the alias)
			// is copied from its newest version.
			if p, ok := merged[d.Path]; !ok || d.UpdatedAt > p.doc.UpdatedAt {
				merged[d.Path] = pick{doc: d, version: versions[j], src: i}
			}
		}
	}
	st.Docs = len(merged)

	paths := make([]string, 0, len(merged))
	for p, m := range merged {
		if recheck && m.version != 0 {
			st.FromStore++
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		m := merged[p]
		row := repository.KBDocument{
			Path: m.doc.Path, Title: m.doc.Title, Tags: m.doc.Tags, Author: m.doc.Author, Reason: m.doc.Reason,
			Content: m.doc.Content, Annotations: m.doc.Annotations,
		}
		if m.doc.UpdatedAt > 0 {
			row.UpdatedAt = time.Unix(m.doc.UpdatedAt, 0)
		}
		ok, err := s.store.InsertKBDocumentIfMissing(ctx, rs, row, backfillBy)
		if err != nil {
			return st, fmt.Errorf("copy %s into postgres: %w", p, err)
		}
		if ok {
			st.Inserted++
			st.Collections[m.src].Inserted++
		} else {
			st.Present++
			st.Collections[m.src].Present++
		}
	}
	for _, cs := range st.Collections {
		s.log.Info("kb backfill collection", "kb", sc.String(), "collection", cs.Collection,
			"found", cs.Found, "inserted", cs.Inserted, "already_present", cs.Present)
	}

	// Verify: every exported path now has a row, live or deleted ...
	have := map[string]bool{}
	if err := s.store.StreamKBDocuments(ctx, rs, func(d repository.KBDocument) error {
		have[d.Path] = true
		return nil
	}); err != nil {
		return st, fmt.Errorf("verify: list rows: %w", err)
	}
	deleted, err := s.store.ListDeletedKBPaths(ctx, rs)
	if err != nil {
		return st, fmt.Errorf("verify: list deleted rows: %w", err)
	}
	for _, p := range deleted {
		have[p] = true
	}
	var missing []string
	for _, p := range paths {
		if !have[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return st, fmt.Errorf("verify: %d of %d exported docs have no postgres row (first: %s)", len(missing), len(paths), missing[0])
	}
	if st.Inserted+st.Present+st.FromStore != st.Docs {
		return st, fmt.Errorf("verify: inserted %d + already present %d + indexed from postgres %d != %d docs found",
			st.Inserted, st.Present, st.FromStore, st.Docs)
	}
	// ... and nothing was added to the index while it was being copied.
	for i, col := range sources {
		n, err := s.countIn(ctx, col)
		if err != nil {
			return st, fmt.Errorf("verify: count %s: %w", col, err)
		}
		if n != counts[i] {
			return st, fmt.Errorf("%w: %s held %d chunk-0 docs at export and %d after the copy", errBackfillUnstable, col, counts[i], n)
		}
	}
	return st, nil
}

// exportChunk0 reads every chunk-0 record (one per doc, with its full
// content and annotations) of collection col through Typesense's export
// endpoint, which streams the whole collection with no page cap.
func (s *Service) exportChunk0(ctx context.Context, col string) ([]*Doc, error) {
	docs, _, err := s.exportChunk0Versions(ctx, col)
	return docs, err
}

// exportChunk0Versions is exportChunk0, also returning each record's
// kb_version (0 when it has none: written before it existed, or by old code).
func (s *Service) exportChunk0Versions(ctx context.Context, col string) ([]*Doc, []int, error) {
	body, err := s.client.Collection(col).Documents().Export(ctx, &api.ExportDocumentsParams{
		FilterBy:      pointer.String("chunk_index:=0"),
		ExcludeFields: pointer.String("embedding"),
	})
	if err != nil {
		return nil, nil, err
	}
	defer body.Close()
	dec := json.NewDecoder(body)
	var out []*Doc
	var versions []int
	for {
		var m map[string]interface{}
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return out, versions, nil
			}
			return nil, nil, fmt.Errorf("decode export line %d: %w", len(out)+1, err)
		}
		d, err := backfillDoc(m)
		if err != nil {
			return nil, nil, err
		}
		v, _ := m[indexedVersionField].(float64)
		out = append(out, d)
		versions = append(versions, int(v))
	}
}

// backfillDoc decodes one exported chunk-0 record. It is strict where
// mapToDoc is lenient: a record without a path or content, or with
// annotations it can't read, fails the KB rather than be copied without
// them (the backfill skips a path once it has a row, so a lossy copy would
// stay lossy).
func backfillDoc(m map[string]interface{}) (*Doc, error) {
	path, _ := m["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("exported record %v has no path", m["id"])
	}
	if _, ok := m["content"].(string); !ok {
		return nil, fmt.Errorf("exported record for %s has no content field", path)
	}
	var anns []Annotation
	switch v := m["annotations"].(type) {
	case nil:
	case string:
		if v != "" {
			if err := json.Unmarshal([]byte(v), &anns); err != nil {
				return nil, fmt.Errorf("annotations of %s: %w", path, err)
			}
		}
	case []interface{}:
		b, err := json.Marshal(v)
		if err == nil {
			err = json.Unmarshal(b, &anns)
		}
		if err != nil {
			return nil, fmt.Errorf("annotations of %s: %w", path, err)
		}
	default:
		return nil, fmt.Errorf("annotations of %s have an unexpected type %T", path, v)
	}
	d := mapToDocFromMap(m)
	d.Annotations = kbAnnotationsOrEmpty(anns)
	return d, nil
}

// kbBaseName matches a KB's base collection or alias name: kb_<project>,
// kb_team_<id>, kb_org_<id>; buildName matches a reindex build of one.
var (
	kbBaseName = regexp.MustCompile(`^kb_(?:(team|org)_)?(\d+)$`)
	buildName  = regexp.MustCompile(`^(kb_(?:(?:team|org)_)?\d+)_v\d+$`)
)

// scopeOfBase is the KB a base collection or alias name belongs to. The
// global KB (kb_global_0) and anything else that isn't a KB name are not.
func scopeOfBase(name string) (Scope, bool) {
	m := kbBaseName.FindStringSubmatch(name)
	if m == nil {
		return Scope{}, false
	}
	id, err := strconv.Atoi(m[2])
	if err != nil {
		return Scope{}, false
	}
	switch m[1] {
	case "":
		return Project(id), true
	case "team":
		return Team(id), true
	default:
		return Org(id), true
	}
}

// discoverKBScopes lists the KBs that have something in Typesense: a real
// collection or an alias under a KB base name, or a reindex build of one.
func (s *Service) discoverKBScopes(ctx context.Context) ([]Scope, error) {
	cols, err := s.client.Collections().Retrieve(ctx, &api.GetCollectionsParams{ExcludeFields: pointer.String("fields")})
	if err != nil {
		return nil, err
	}
	aliases, err := s.client.Aliases().Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	names := make([]string, 0, len(cols)+len(aliases))
	for _, c := range cols {
		names = append(names, c.Name)
	}
	for _, a := range aliases {
		if a.Name != nil {
			names = append(names, *a.Name)
		}
	}
	seen := map[Scope]bool{}
	var out []Scope
	for _, n := range names {
		if m := buildName.FindStringSubmatch(n); m != nil {
			n = m[1]
		}
		sc, ok := scopeOfBase(n)
		if !ok || seen[sc] {
			continue
		}
		seen[sc] = true
		out = append(out, sc)
	}
	return out, nil
}

// backfillDone reports whether sc's backfill has completed. A KB that is not
// complete but has no search collection at all (a new project, team or org)
// has nothing to copy, so it is marked complete here, the first time the
// service touches it. That needs Typesense healthy (GET /health ok) before
// it is asked: one still loading may not list a collection it has. If
// Typesense is not healthy or can't be asked, it answers not complete.
func (s *Service) backfillDone(ctx context.Context, sc Scope) (bool, error) {
	rs := storeScope(sc)
	done, err := s.store.IsKBBackfillComplete(ctx, rs)
	if err != nil || done {
		return done, err
	}
	if err := s.typesenseHealthy(ctx); err != nil {
		s.log.Info("kb backfill: not marking a KB with no collection complete; typesense is not healthy", "kb", sc.String(), "err", err)
		return false, nil
	}
	kc, err := s.kbCollections(ctx, collectionName(sc))
	if err != nil || kc.real || kc.alias != "" {
		return false, nil
	}
	if err := s.store.CompleteKBBackfill(ctx, rs, 0); err != nil {
		s.log.Warn("kb backfill: could not mark a KB with no collection complete", "kb", sc.String(), "err", err)
		return false, nil
	}
	s.log.Info("kb backfill: no search collection, nothing to copy; marked complete", "kb", sc.String())
	return true, nil
}
