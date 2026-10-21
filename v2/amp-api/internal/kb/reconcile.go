package kb

// The reconciler brings each stored KB's search index back in line with its
// Postgres rows (decisions/kb-postgres-source-of-truth.md). A write commits
// to Postgres first and indexes afterwards; if the indexing fails (Typesense
// down, a queue job lost), the doc is saved but missing or stale in search
// until something re-indexes it. The reconciler is that something.
//
// It writes Typesense. It writes Postgres only to keep a change that exists
// nowhere else, and never deletes a row:
//   - a live row that the index lacks, or holds an older copy of, is
//     re-indexed from the row (indexFromStore);
//   - an entry an old (pre-Postgres) pod edited or annotated after the row
//     was written (no kb_version, updated_at newer than the row's, or equal
//     with annotations the row lacks; see oldPodEdit) is copied into the row
//     first (UpsertKBDocument, so the previous content stays in
//     kb_document_versions, and a union of the annotations), then
//     re-indexed from it. This covers a rolling deploy and a rollback (#2027);
//   - an index entry whose row is soft-deleted is removed;
//   - an index entry with no row at all, written by an old pod during a
//     rolling deploy (see adoptable), is copied into Postgres insert-only
//     (copyIndexed: InsertKBDocumentIfMissing, keeping its updated_at and
//     annotations) and then re-indexed from the new row. Any other entry
//     with no row is left alone and only logged: it may be the last copy of
//     something, and deleting it is never the reconciler's call.
//
// It works only on KBs whose backfill has completed, so indexFromStore's
// adopt-from-index step never runs. Each KB is
// reconciled under its reindex lock, so it never runs alongside a reindex or
// a Drop of the same KB; a KB whose lock is taken is skipped until the next
// pass. RunReconciler holds a cross-replica lock for the whole pass, so only
// one replica reconciles at a time.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// reconcileAdoptBy is the changed_by recorded on a row the reconciler
// copies in from an index-only entry.
const reconcileAdoptBy = "kb-reconcile-adopt"

// reconcileEditBy is the changed_by recorded when the reconciler copies an
// old pod's edit of an existing doc into its row.
const reconcileEditBy = "kb-reconcile-old-pod-edit"

// adoptClockSlack allows for clock skew between the pod that wrote an
// index-only entry (its updated_at is that pod's clock) and Postgres (the
// backfill's started_at is NOW() there).
const adoptClockSlack = 5 * time.Minute

// ReconcileStats is what one KB's reconcile found and fixed.
type ReconcileStats struct {
	Scope   Scope
	Live    int // live rows in Postgres
	Indexed int // docs in the index (chunk 0 entries)
	Missing int // live rows the index lacked, re-indexed
	Stale   int // live rows the index held an older copy of, re-indexed
	Removed int // index entries of soft-deleted rows, removed
	// Adopted counts index entries with no Postgres row at all that were
	// copied into Postgres (insert-only) and re-indexed from the new row.
	Adopted int
	// IndexOnly counts index entries with no Postgres row at all that were
	// not adopted (see adoptable). They are left in place and logged.
	IndexOnly int
	// AdoptedEdits counts live rows that an old pod had edited or annotated
	// in the index only: the edit was copied into the row (and the entry
	// re-indexed from it) instead of being overwritten.
	AdoptedEdits int
	Failed       int // fixes that failed; retried by the next pass
	// Skipped says why the KB was not reconciled ("" = it was).
	Skipped string
}

// Fixed is the number of docs the reconcile changed in the index.
func (st ReconcileStats) Fixed() int { return st.Missing + st.Stale + st.Removed }

// indexState is what the index holds for one doc, as the reconciler
// compares it with the row.
type indexState struct {
	key       string // see stateKey
	version   int    // 0 when the entry predates indexedVersionField
	updatedAt int64  // unix seconds
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
		key:       stateKey(d.Title, d.Author, d.Reason, d.Tags, d.Annotations, d.UpdatedAt.Unix()),
		version:   d.Version,
		updatedAt: d.UpdatedAt.Unix(),
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
	var fixed, adopted, edits, failed, skipped int
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
		adopted += st.Adopted
		edits += st.AdoptedEdits
		failed += st.Failed
		if st.Skipped != "" {
			skipped++
		}
		out = append(out, st)
	}
	s.log.Info("kb reconcile pass done", "kbs", len(scopes), "fixed", fixed, "adopted", adopted, "adopted_edits", edits, "failed", failed, "skipped", skipped)
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

	var toIndex, edited []string
	for path, want := range live {
		ix, ok := indexed[path]
		switch {
		case !ok:
			st.Missing++
			toIndex = append(toIndex, path)
		case !ix.current(want) && oldPodEdit(ix, want):
			edited = append(edited, path)
		case !ix.current(want):
			st.Stale++
			toIndex = append(toIndex, path)
		}
	}
	for _, path := range edited {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		switch adopted, err := s.adoptEdit(ctx, sc, path); {
		case err != nil:
			// Leave the entry alone: re-indexing it from the row would
			// overwrite the edit this pass failed to save.
			st.Failed++
			s.log.Warn("kb reconcile: could not copy an old pod's edit into postgres; the next pass retries",
				"kb", sc.String(), "path", path, "err", err)
		case adopted:
			st.AdoptedEdits++
			toIndex = append(toIndex, path)
		default:
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
		// Deleted rows' entries go; entries with no row at all are adopted
		// or left in place.
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
				orphans = append(orphans, p)
			}
		}
		if len(orphans) > 0 {
			again, err := s.adoptOrphans(ctx, sc, orphans, indexed, &st)
			if err != nil {
				return st, err
			}
			toIndex = append(toIndex, again...)
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
	if st.Fixed() > 0 || st.Adopted > 0 || st.AdoptedEdits > 0 || st.Failed > 0 || st.IndexOnly > 0 {
		s.log.Info("kb reconciled", "kb", sc.String(), "live", st.Live, "indexed", st.Indexed,
			"missing", st.Missing, "stale", st.Stale, "removed", st.Removed,
			"adopted", st.Adopted, "adopted_edits", st.AdoptedEdits, "index_only", st.IndexOnly, "failed", st.Failed)
	}
	return st, nil
}

// adoptOrphans handles index entries whose path has no Postgres row, live
// or deleted. Each adoptable one is copied into Postgres insert-only and
// re-indexed from the new row; the rest are counted as IndexOnly and left
// in place. It returns paths that gained a row since the listing (a write
// or delete raced the reconcile): the caller re-indexes those from their
// row like any other. The error is only for failing to read the backfill
// record; a failed copy is counted in st.Failed and retried next pass.
func (s *Service) adoptOrphans(ctx context.Context, sc Scope, orphans []string, indexed map[string]indexState, st *ReconcileStats) ([]string, error) {
	b, err := s.store.GetKBBackfill(ctx, storeScope(sc))
	if err != nil {
		return nil, fmt.Errorf("read the backfill record: %w", err)
	}
	var again, left []string
	for _, p := range orphans {
		if err := ctx.Err(); err != nil {
			return again, err
		}
		if !adoptable(indexed[p], b.StartedAt) {
			left = append(left, p)
			continue
		}
		inserted, found, err := s.copyIndexed(ctx, sc, p, reconcileAdoptBy)
		switch {
		case err != nil:
			st.Failed++
			s.log.Warn("kb reconcile: could not copy an index-only doc into postgres; the next pass retries",
				"kb", sc.String(), "path", p, "err", err)
			continue
		case !found:
			continue // the entry went away since the listing
		case !inserted:
			// A row appeared since the listing; index from it (or remove
			// the entry if that row is deleted).
			again = append(again, p)
			continue
		}
		st.Adopted++
		s.log.Info("kb reconcile: copied an index-only doc into postgres",
			"kb", sc.String(), "path", p, "updated_at", indexed[p].updatedAt, "changed_by", reconcileAdoptBy)
		// Re-index from the new row, so the entry carries its kb_version.
		if err := s.indexFromStore(ctx, sc, p); err != nil {
			st.Failed++
			s.log.Warn("kb reconcile: re-index of an adopted doc failed", "kb", sc.String(), "path", p, "err", err)
		}
	}
	st.IndexOnly += len(left)
	if len(left) > 0 {
		s.log.Warn("kb reconcile: index entries with no postgres row were left in place (not written by an old pod after the backfill)",
			"kb", sc.String(), "count", len(left), "first", left[0])
	}
	return again, nil
}

// oldPodEdit reports whether a stale index entry of a live row may hold an
// edit or annotation that only an old (pre-Postgres) pod made, which
// re-indexing from the row would destroy. New pods always index from the row
// with its own updated_at and stamp kb_version, and backfilled entries carry
// their row's updated_at, so an entry with no kb_version dated after its row
// can only come from old code: a write (old WriteDoc dates it now) during a
// rolling deploy or after a rollback. Old code's annotate keeps the doc's
// updated_at, so an entry dated the same second as its row may hold new
// annotations too; adoptEdit decides that by comparing them. An entry older
// than its row is the normal stale case (an index job failed after a new
// write) and is re-indexed from the row.
func oldPodEdit(ix, row indexState) bool {
	return ix.version == 0 && ix.updatedAt >= row.updatedAt
}

// adoptEdit copies an old pod's index-only edit of path into its live row:
// the entry's content, title, tags, author and reason when the entry is
// newer than the row (UpsertKBDocument, so the row's previous content stays
// in kb_document_versions), and any annotations the entry has that the row
// lacks (UpdateKBAnnotations, see mergeAnnotations). Both the row and the
// entry are re-read, so a change since the listing is judged as it is now; a
// new-pod write racing this one wins (ExpectVersion), and the entry is then
// re-indexed from it. It reports whether anything was copied; false means
// the entry is just stale and the caller re-indexes it from the row.
func (s *Service) adoptEdit(ctx context.Context, sc Scope, path string) (bool, error) {
	entry, err := s.getIndexed(ctx, sc, path)
	if isNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the index entry: %w", err)
	}
	entry.Path = path
	return s.adoptEditEntry(ctx, sc, entry, reconcileEditBy)
}

// adoptEditEntry is adoptEdit for an index entry the caller has already
// read (a reindex reads it through export, from a collection the base name
// may no longer point at). The row is re-read here; the edit is recorded as
// changed by by.
func (s *Service) adoptEditEntry(ctx context.Context, sc Scope, entry *Doc, by string) (bool, error) {
	rs := storeScope(sc)
	path := entry.Path
	row, err := s.store.GetKBDocument(ctx, rs, path)
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil // deleted since the listing: re-indexing removes the entry
	}
	if err != nil {
		return false, fmt.Errorf("read the row: %w", err)
	}
	rowUpdated := row.UpdatedAt.Unix()
	if entry.UpdatedAt < rowUpdated {
		return false, nil // the row moved on since the listing
	}
	var wrote bool
	if entry.UpdatedAt > rowUpdated {
		_, changed, err := s.store.UpsertKBDocument(ctx, rs, repository.KBDocWrite{
			Path: path, Title: entry.Title, Tags: entry.Tags, Author: entry.Author, Reason: entry.Reason,
			Content: entry.Content, By: by, ExpectVersion: row.Version,
		})
		if errors.Is(err, repository.ErrConflict) {
			return false, nil // a new write landed since the read; it wins
		}
		if err != nil {
			return false, fmt.Errorf("save the edit: %w", err)
		}
		wrote = changed
	}
	added := 0
	if len(mergeAnnotations(row.Annotations, entry.Annotations)) > len(kbAnnotationsOrEmpty(row.Annotations)) {
		if _, err := s.store.UpdateKBAnnotations(ctx, rs, path, func(cur []Annotation) ([]Annotation, error) {
			next := mergeAnnotations(cur, entry.Annotations)
			added = len(next) - len(cur)
			return next, nil
		}); err != nil {
			return false, fmt.Errorf("merge the annotations: %w", err)
		}
	}
	if !wrote && added == 0 {
		return false, nil
	}
	s.log.Info("kb: copied an old pod's edit into postgres instead of overwriting it",
		"kb", sc.String(), "path", path, "entry_updated_at", entry.UpdatedAt, "row_updated_at", rowUpdated,
		"content_saved", wrote, "annotations_added", added, "changed_by", by)
	return true, nil
}

// mergeAnnotations is the union of a row's annotations and an index entry's.
// Annotations have no id, so one is identified by author, created_at and
// text. The row's copy wins where both have it (its resolved state is kept),
// none of the row's is removed, and the result stays oldest first.
func mergeAnnotations(row, entry []Annotation) []Annotation {
	type annKey struct {
		author, text string
		at           int64
	}
	out := slices.Clone(kbAnnotationsOrEmpty(row))
	seen := make(map[annKey]bool, len(out))
	for _, a := range out {
		seen[annKey{a.Author, a.Text, a.CreatedAt}] = true
	}
	added := false
	for _, a := range entry {
		k := annKey{a.Author, a.Text, a.CreatedAt}
		if !seen[k] {
			seen[k] = true
			out = append(out, a)
			added = true
		}
	}
	if added {
		slices.SortStableFunc(out, func(a, b Annotation) int { return cmp.Compare(a.CreatedAt, b.CreatedAt) })
	}
	return out
}

// adoptable reports whether an index entry with no Postgres row is one the
// reconciler copies into Postgres: a doc an old (pre-Postgres) pod wrote
// straight to Typesense during a rolling deploy, after the KB's backfill
// had read the index. Such an entry
//   - has no kb_version: new pods index only from a row and always stamp
//     it, so an entry with a version and no row had a row once that is now
//     gone (purged, or deleted by hand), and must not come back;
//   - is dated no earlier than the backfill's latest start (less
//     adoptClockSlack): anything older was in the index when the backfill
//     exported it, so it was copied then, and has no row now only because it
//     was purged after a delete whose index removal never happened.
func adoptable(ix indexState, backfillStarted time.Time) bool {
	if ix.version != 0 {
		return false
	}
	return ix.updatedAt >= backfillStarted.Add(-adoptClockSlack).Unix()
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
			ix := indexState{key: stateKey(d.Title, d.Author, d.Reason, d.Tags, d.Annotations, d.UpdatedAt), updatedAt: d.UpdatedAt}
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

// reindexAdoptBy and reindexEditBy are the changed_by recorded when a
// reindex copies an old pod's index-only doc, or its edit of a live doc,
// into Postgres before an old collection is deleted.
const (
	reindexAdoptBy = "kb-reindex-adopt"
	reindexEditBy  = "kb-reindex-old-pod-edit"
)

// adoptOldPodWrites is the reconciler's adoption, run by a reindex (which
// holds sc's reindex lock) on collections it is about to delete: every
// index-only change an old (pre-Postgres) pod made in them is copied into
// Postgres first, so a build from Postgres keeps it. It reads each
// collection through the export endpoint, which streams every record, so a
// write reordering the collection can't make it skip one (paged search
// could). Per path, the newest entry across cols is judged:
//   - a live row whose entry is an old pod's edit or annotation (oldPodEdit)
//     gets it through adoptEditEntry (UpsertKBDocument and an annotation
//     union, so nothing of the row is lost);
//   - a path with no row at all whose entry is adoptable is copied in
//     insert-only (insertIndexedDoc);
//   - a soft-deleted row's entry is stale and left out of the build.
//
// It returns the paths it could not account for: entries with no row that
// are not adoptable (only collected when strict; the caller then refuses to
// delete those collections). Every entry is judged before anything is
// written, so when strict finds such a path nothing is copied: a refused
// reindex leaves Postgres as it was. A Postgres or export error is returned
// as is; what was copied before it stays (insert-only or versioned, so
// harmless).
func (s *Service) adoptOldPodWrites(ctx context.Context, sc Scope, cols []string, strict bool) (st ReconcileStats, unadopted []string, err error) {
	st.Scope = sc
	if len(cols) == 0 {
		return st, nil, nil
	}
	rs := storeScope(sc)
	type entry struct {
		d *Doc
		v int
	}
	newest := map[string]entry{}
	for _, col := range cols {
		docs, versions, err := s.exportChunk0Versions(ctx, col)
		if err != nil {
			return st, nil, fmt.Errorf("export %s: %w", col, err)
		}
		for i, d := range docs {
			if e, ok := newest[d.Path]; !ok || d.UpdatedAt > e.d.UpdatedAt {
				newest[d.Path] = entry{d, versions[i]}
			}
		}
	}
	st.Indexed = len(newest)
	if len(newest) == 0 {
		return st, nil, nil
	}
	live := map[string]indexState{}
	if err := s.store.StreamKBDocuments(ctx, rs, func(d repository.KBDocument) error {
		live[d.Path] = rowState(&d)
		return nil
	}); err != nil {
		return st, nil, fmt.Errorf("list postgres rows: %w", err)
	}
	st.Live = len(live)
	deletedPaths, err := s.store.ListDeletedKBPaths(ctx, rs)
	if err != nil {
		return st, nil, fmt.Errorf("list deleted rows: %w", err)
	}
	deleted := make(map[string]bool, len(deletedPaths))
	for _, p := range deletedPaths {
		deleted[p] = true
	}
	paths := make([]string, 0, len(newest))
	for p := range newest {
		paths = append(paths, p)
	}
	slices.Sort(paths)

	// Judge every entry first, so a refusal (strict, something left
	// unadopted) writes nothing to Postgres.
	var edits, orphans []string
	var backfillStarted *time.Time
	for _, p := range paths {
		e := newest[p]
		d := e.d
		ix := indexState{key: stateKey(d.Title, d.Author, d.Reason, d.Tags, d.Annotations, d.UpdatedAt), version: e.v, updatedAt: d.UpdatedAt}
		if want, ok := live[p]; ok {
			if !ix.current(want) && oldPodEdit(ix, want) {
				edits = append(edits, p)
			}
			continue
		}
		if deleted[p] {
			continue
		}
		if backfillStarted == nil {
			b, err := s.store.GetKBBackfill(ctx, rs)
			if err != nil {
				return st, nil, fmt.Errorf("read the backfill record: %w", err)
			}
			backfillStarted = &b.StartedAt
		}
		if adoptable(ix, *backfillStarted) {
			orphans = append(orphans, p)
			continue
		}
		st.IndexOnly++
		if strict {
			unadopted = append(unadopted, p)
		}
	}
	if len(unadopted) > 0 {
		return st, unadopted, nil
	}
	for _, p := range edits {
		if err := ctx.Err(); err != nil {
			return st, nil, err
		}
		adopted, err := s.adoptEditEntry(ctx, sc, newest[p].d, reindexEditBy)
		if err != nil {
			return st, nil, fmt.Errorf("copy an old pod's edit of %s into postgres: %w", p, err)
		}
		if adopted {
			st.AdoptedEdits++
		}
	}
	for _, p := range orphans {
		if err := ctx.Err(); err != nil {
			return st, nil, err
		}
		d := newest[p].d
		inserted, err := s.insertIndexedDoc(ctx, sc, d, reindexAdoptBy)
		if err != nil {
			return st, nil, fmt.Errorf("copy the index-only %s: %w", p, err)
		}
		if inserted {
			st.Adopted++
			s.log.Info("kb reindex: copied an old pod's index-only doc into postgres before deleting its collection",
				"kb", sc.String(), "path", p, "updated_at", d.UpdatedAt, "changed_by", reindexAdoptBy)
		}
		// Not inserted: a row appeared since the listing, so the build has it.
	}
	return st, unadopted, nil
}
