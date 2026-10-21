package kb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// importBy is changed_by on the version row an imported doc starts with.
const importBy = "kb-import"

// ErrDocExists is returned by ImportDoc when the KB already has a row for the
// path (live or deleted). The existing doc is left as it was.
var ErrDocExists = errors.New("kb doc already exists")

// ExportDocs returns every live doc of a KB for a project export: content,
// tags, author, reason, annotations and the original dates, most recently
// updated first. A stored KB is read from Postgres, plus (until its backfill
// completes) the docs that so far exist only in the index; a KB without a
// store is read from the index. Every doc is listed, with no page cap. It
// fails rather than return part of the KB: an export that silently left docs
// out would lose them on import.
func (s *Service) ExportDocs(ctx context.Context, sc Scope) ([]domain.ExportKBDoc, error) {
	if !s.stored(sc) {
		return s.exportIndexed(ctx, sc, nil, nil)
	}
	rows, err := s.store.ListKBDocuments(ctx, storeScope(sc), 0, 0)
	if err != nil {
		return nil, fmt.Errorf("list docs of %s: %w", sc, err)
	}
	out := make([]domain.ExportKBDoc, 0, len(rows))
	have := make(map[string]bool, len(rows))
	for i := range rows {
		have[rows[i].Path] = true
		out = append(out, exportFromRow(&rows[i]))
	}
	done, deleted, err := s.storedRead(ctx, sc)
	if err != nil {
		return nil, err
	}
	if done {
		return out, nil
	}
	indexed, err := s.exportIndexed(ctx, sc, have, deleted)
	if err != nil {
		return nil, err
	}
	if len(indexed) > 0 {
		out = append(out, indexed...)
		sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	}
	return out, nil
}

// exportIndexed reads every doc the index holds, skipping the paths in skip
// (Postgres has them) and deleted (an index entry for a deleted row is stale).
func (s *Service) exportIndexed(ctx context.Context, sc Scope, skip, deleted map[string]bool) ([]domain.ExportKBDoc, error) {
	docs, err := s.listIndexed(ctx, sc, "")
	if err != nil {
		return nil, err
	}
	out := make([]domain.ExportKBDoc, 0, len(docs))
	for _, d := range docs {
		if skip[d.Path] || deleted[d.Path] {
			continue
		}
		full, err := s.getIndexed(ctx, sc, d.Path)
		if err != nil {
			if isNotFound(err) {
				continue // deleted since the listing
			}
			return nil, fmt.Errorf("read %s/%s for export: %w", collectionName(sc), d.Path, err)
		}
		out = append(out, domain.ExportKBDoc{
			Path: full.Path, Title: full.Title, Content: full.Content, Tags: full.Tags, Author: full.Author,
			Annotations: full.Annotations, Reason: full.Reason, UpdatedAt: full.UpdatedAt,
		})
	}
	return out, nil
}

func exportFromRow(d *repository.KBDocument) domain.ExportKBDoc {
	tags := d.Tags
	if tags == nil {
		tags = []string{}
	}
	return domain.ExportKBDoc{
		Path: d.Path, Title: d.Title, Content: d.Content, Tags: tags, Author: d.Author,
		Annotations: d.Annotations, Reason: d.Reason,
		CreatedAt: d.CreatedAt.Unix(), UpdatedAt: d.UpdatedAt.Unix(),
	}
}

// ImportDoc writes one doc from a project export into sc, keeping its
// author, reason, annotations and dates. A normal write would date the doc
// now, so a stored KB inserts the row directly (InsertKBDocumentIfMissing,
// recording version 1 by "kb-import") and then indexes it from the committed
// row. An existing row for the path, live or deleted, is left alone and
// ErrDocExists returned. A zero UpdatedAt (an export made before dates were
// exported) becomes now, and a zero CreatedAt becomes UpdatedAt.
//
// A KB without a store goes through WriteDoc, as imports always did there,
// so its docs are dated now.
func (s *Service) ImportDoc(ctx context.Context, sc Scope, d domain.ExportKBDoc) error {
	if d.Path == "" {
		return fmt.Errorf("import: kb doc has no path")
	}
	if !s.stored(sc) {
		_, err := s.WriteDoc(ctx, sc, d.Path, d.Title, d.Content, d.Author, d.Tags, d.Reason, d.Annotations)
		return err
	}
	// A doc only the index has (before the backfill) counts as existing.
	if err := s.adoptIndexed(ctx, sc, d.Path); err != nil {
		return err
	}
	row := repository.KBDocument{
		Path: d.Path, Title: d.Title, Tags: d.Tags, Author: d.Author, Reason: d.Reason,
		Content: d.Content, Annotations: d.Annotations,
	}
	if d.UpdatedAt > 0 {
		row.UpdatedAt = time.Unix(d.UpdatedAt, 0)
	}
	if d.CreatedAt > 0 {
		row.CreatedAt = time.Unix(d.CreatedAt, 0)
	}
	inserted, err := s.store.InsertKBDocumentIfMissing(ctx, storeScope(sc), row, importBy)
	if err != nil {
		return fmt.Errorf("import %s into %s: %w", d.Path, sc, err)
	}
	if !inserted {
		return fmt.Errorf("import %s into %s: %w", d.Path, sc, ErrDocExists)
	}
	s.index(ctx, sc, d.Path)
	return nil
}
