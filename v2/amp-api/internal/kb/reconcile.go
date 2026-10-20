package kb

// The reconciler brings each stored KB's search index back in line with its
// Postgres rows (decisions/kb-postgres-source-of-truth.md). A write commits
// to Postgres first and indexes afterwards; if the indexing fails (Typesense
// down, a queue job lost), the doc is saved but missing or stale in search
// until something re-indexes it. The reconciler is that something.
//
// It only ever reads Postgres and writes Typesense:
//   - a live row that the index lacks, or holds an older copy of, is
//     re-indexed from the row (indexFromStore);
//   - an index entry whose row is soft-deleted is removed;
//   - an index entry with no row at all is left alone and only logged: it
//     may be the last copy of something, and deleting it is never the
//     reconciler's call.
//
// It works only on KBs whose backfill has completed, so every doc is in
// Postgres and indexFromStore's adopt-from-index step never runs (that step
// is the one path in indexFromStore that writes Postgres). Each KB is
// reconciled under its reindex lock, so it never runs alongside a reindex or
// a Drop of the same KB; a KB whose lock is taken is skipped until the next
// pass. RunReconciler holds a cross-replica lock for the whole pass, so only
// one replica reconciles at a time.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/typesense/typesense-go/v4/typesense/api"
	"github.com/typesense/typesense-go/v4/typesense/api/pointer"

	"github.com/simstech/amp-api/internal/repository"
)

// indexedVersionField holds the Postgres row version on each index entry.
// It is not in the collection schema: Typesense stores it without indexing
// it, and entries written before it existed simply lack it.
const indexedVersionField = "kb_version"

// reconcileLockKey is the cross-replica lock a reconcile pass holds.
const reconcileLockKey = "kb_reconcile"

// ReconcileStats is what one KB's reconcile found and fixed.
type ReconcileStats struct {
	Scope   Scope
	Live    int // live rows in Postgres
	Indexed int // docs in the index (chunk 0 entries)
	Missing int // live rows the index lacked, re-indexed
	Stale   int // live rows the index held an older copy of, re-indexed
	Removed int // index entries of soft-deleted rows, removed
	// IndexOnly counts index entries with no Postgres row at all. They are
	// left in place and logged.
	IndexOnly int
	Failed    int // fixes that failed; retried by the next pass
	// Skipped says why the KB was not reconciled ("" = it was).
	Skipped string
}

// Fixed is the number of docs the reconcile changed in the index.
func (st ReconcileStats) Fixed() int { return st.Missing + st.Stale + st.Removed }

// indexState is what the index holds for one doc, as the reconciler
// compares it with the row.
type indexState struct {
	key     string // see stateKey
	version int    // 0 when the entry predates indexedVersionField
}

// stateKey is everything an index entry carries about its row apart from
// content and version. Content is not listed (it would mean reading every
// doc's full text each pass); a content change moves updated_at, and the
// version, when the entry has one, catches a change within the same second.
func stateKey(title, author, reason string, tags []string, anns []Annotation, updatedAt int64) string {
	if tags == nil {
		tags = []string{}
	}
	t, _ := json.Marshal(tags)
	a, _ := json.Marshal(kbAnnotationsOrEmpty(anns))
	return fmt.Sprintf("%d|%q|%q|%q|%s|%s", updatedAt, title, author, reason, t, a)
}

func rowState(d *repository.KBDocument) indexState {
	return indexState{
		key:     stateKey(d.Title, d.Author, d.Reason, d.Tags, d.Annotations, d.UpdatedAt.Unix()),
		version: d.Version,
	}
}

// current reports whether the index entry ix matches the row state want.
func (ix indexState) current(want indexState) bool {
	if ix.key != want.key {
		return false
	}
	return ix.version == 0 || ix.version == want.version
}

// ReconcileAll reconciles every KB whose backfill has completed and returns
// what each one found. A KB that fails is logged and the pass carries on;
// the error is only for failing to list the KBs, or ctx ending.
func (s *Service) ReconcileAll(ctx context.Context) ([]ReconcileStats, error) {
	if s.store == nil {
		return nil, nil
	}
	scopes, err := s.store.ListBackfilledKBScopes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list backfilled KBs: %w", err)
	}
	out := make([]ReconcileStats, 0, len(scopes))
	var fixed, failed, skipped int
	for _, rs := range scopes {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		sc := Scope{Kind: rs.Kind, ID: rs.ID}
		if s.reconcileOnly != nil && !s.reconcileOnly(sc) {
			continue
		}
		st, err := s.ReconcileKB(ctx, sc)
		if err != nil {
			st.Failed++
			s.log.Warn("kb reconcile failed; the next pass retries", "kb", sc.String(), "err", err)
		}
		fixed += st.Fixed()
		failed += st.Failed
		if st.Skipped != "" {
			skipped++
		}
		out = append(out, st)
	}
	s.log.Info("kb reconcile pass done", "kbs", len(scopes), "fixed", fixed, "failed", failed, "skipped", skipped)
	return out, nil
}

// ReconcileKB brings one KB's index in line with its Postgres rows. It does
// nothing (Skipped set, nil error) when the KB isn't stored, isn't
// backfilled, or a reindex or Drop of it holds its lock.
func (s *Service) ReconcileKB(ctx context.Context, sc Scope) (ReconcileStats, error) {
	st := ReconcileStats{Scope: sc}
	if !s.stored(sc) {
		st.Skipped = "not stored in postgres"
		return st, nil
	}
	rs := storeScope(sc)
	done, err := s.store.IsKBBackfillComplete(ctx, rs)
	if err != nil {
		return st, fmt.Errorf("check the backfill: %w", err)
	}
	if !done {
		// Postgres may not hold every doc yet: the index may have the only
		// copy, and indexFromStore would adopt it into Postgres.
		st.Skipped = "backfill not complete"
		return st, nil
	}
	release, err := s.beginReindex(ctx, sc)
	if errors.Is(err, ErrReindexRunning) {
		st.Skipped = "reindex or drop running"
		s.log.Info("kb reconcile skipped: a reindex or drop holds the lock", "kb", sc.String())
		return st, nil
	}
	if err != nil {
		return st, err
	}
	defer release()

	live := map[string]indexState{}
	if err := s.store.StreamKBDocuments(ctx, rs, func(d repository.KBDocument) error {
		live[d.Path] = rowState(&d)
		return nil
	}); err != nil {
		return st, fmt.Errorf("list postgres rows: %w", err)
	}
	st.Live = len(live)
	indexed, err := s.listIndexState(ctx, sc)
	if err != nil {
		return st, fmt.Errorf("list the index: %w", err)
	}
	st.Indexed = len(indexed)

	var toIndex []string
	for path, want := range live {
		ix, ok := indexed[path]
		switch {
		case !ok:
			st.Missing++
			toIndex = append(toIndex, path)
		case !ix.current(want):
			st.Stale++
			toIndex = append(toIndex, path)
		}
	}
	var gone []string
	for path := range indexed {
		if _, ok := live[path]; !ok {
			gone = append(gone, path)
		}
	}
	if len(gone) > 0 {
		// Deleted rows' entries go; entries with no row at all stay.
		deletedPaths, err := s.store.ListDeletedKBPaths(ctx, rs)
		if err != nil {
			return st, fmt.Errorf("list deleted rows: %w", err)
		}
		deleted := make(map[string]bool, len(deletedPaths))
		for _, p := range deletedPaths {
			deleted[p] = true
		}
		var orphans []string
		for _, p := range gone {
			if deleted[p] {
				st.Removed++
				toIndex = append(toIndex, p) // indexFromStore removes it
			} else {
				st.IndexOnly++
				orphans = append(orphans, p)
			}
		}
		if len(orphans) > 0 {
			s.log.Warn("kb reconcile: index entries with no postgres row were left in place",
				"kb", sc.String(), "count", len(orphans), "first", orphans[0])
		}
	}

	for _, path := range toIndex {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		// indexFromStore re-reads the row, so a change since the listing is
		// indexed as it is now, and a row deleted since is removed.
		if err := s.indexFromStore(ctx, sc, path); err != nil {
			st.Failed++
			s.log.Warn("kb reconcile: re-index failed", "kb", sc.String(), "path", path, "err", err)
		}
	}
	if st.Fixed() > 0 || st.Failed > 0 || st.IndexOnly > 0 {
		s.log.Info("kb reconciled", "kb", sc.String(), "live", st.Live, "indexed", st.Indexed,
			"missing", st.Missing, "stale", st.Stale, "removed", st.Removed,
			"index_only", st.IndexOnly, "failed", st.Failed)
	}
	return st, nil
}

// listIndexState lists every doc the index holds (chunk 0 entries, through
// the base name), page by page, without content. A missing collection lists
// as empty.
func (s *Service) listIndexState(ctx context.Context, sc Scope) (map[string]indexState, error) {
	col := collectionName(sc)
	const perPage = 250
	out := map[string]indexState{}
	for page := 1; ; page++ {
		q, pp, pg := "*", perPage, page
		res, err := s.client.Collection(col).Documents().Search(ctx, &api.SearchCollectionParams{
			Q:             &q,
			QueryBy:       pointer.String("title"),
			FilterBy:      pointer.String("chunk_index:=0"),
			SortBy:        pointer.String("updated_at:desc"),
			PerPage:       &pp,
			Page:          &pg,
			ExcludeFields: pointer.String("content,embedding,chunk_text"),
		})
		if err != nil {
			if isNotFound(err) {
				return out, nil
			}
			return nil, err
		}
		if res.Hits == nil {
			return out, nil
		}
		for _, hit := range *res.Hits {
			if hit.Document == nil {
				continue
			}
			m := *hit.Document
			d := mapToDocFromMap(m)
			ix := indexState{key: stateKey(d.Title, d.Author, d.Reason, d.Tags, d.Annotations, d.UpdatedAt)}
			if v, ok := m[indexedVersionField].(float64); ok {
				ix.version = int(v)
			}
			out[d.Path] = ix
		}
		if len(*res.Hits) < perPage {
			return out, nil
		}
	}
}

// RunReconciler reconciles every backfilled KB now and then every interval
// until ctx ends. lock (e.g. repository.TryNamedLock) is a lock shared by
// every replica: a replica that can't take it skips the pass, so only one
// reconciles at a time. With a nil lock every pass runs (single replica).
func (s *Service) RunReconciler(ctx context.Context, interval time.Duration, lock ReindexLocker) {
	if s.store == nil || interval <= 0 {
		return
	}
	s.log.Info("kb reconciler started", "interval", interval.String())
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		s.reconcilePass(ctx, lock)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// reconcilePass runs one ReconcileAll under the cross-replica lock.
func (s *Service) reconcilePass(ctx context.Context, lock ReindexLocker) {
	if lock != nil {
		release, ok, err := lock(ctx, reconcileLockKey)
		if err != nil {
			s.log.Warn("kb reconcile: could not take the reconcile lock; skipping this pass", "err", err)
			return
		}
		if !ok {
			s.log.Debug("kb reconcile: another replica is reconciling; skipping this pass")
			return
		}
		defer release()
	}
	if _, err := s.ReconcileAll(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("kb reconcile pass failed", "err", err)
	}
}
