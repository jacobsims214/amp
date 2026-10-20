package repository

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/domain"
)

// KB documents in Postgres (migration 022, decisions/kb-postgres-source-of-truth.md).
// Postgres is the source of truth for project, team and org KB docs; Typesense
// is a search index rebuilt from these rows. The global KB is not stored here.
//
// Every doc has one row per (scope_kind, scope_id, path), soft-deleted rows
// included: writing a deleted path again revives that row, so its history
// stays in one place. kb_documents.version is the latest version number; a
// version row is written for each change to content, title or tags, and for
// each delete and restore. Annotation changes are not versioned.

// KB scope kinds stored in kb_documents.
const (
	KBScopeProject = "project"
	KBScopeTeam    = "team"
	KBScopeOrg     = "org"
)

// Version change kinds (kb_document_versions.change).
const (
	kbChangeCreate   = "create"
	kbChangeUpdate   = "update"
	kbChangeDelete   = "delete"
	kbChangeRestore  = "restore"
	kbChangeBackfill = "backfill"
)

// KBScope names one knowledge base: a project's, a team's or an org's.
type KBScope struct {
	Kind string
	ID   int
}

func (s KBScope) String() string { return fmt.Sprintf("%s %d", s.Kind, s.ID) }

func (s KBScope) validate() error {
	switch s.Kind {
	case KBScopeProject, KBScopeTeam, KBScopeOrg:
		return nil
	}
	return fmt.Errorf("kb scope %q is not stored in postgres (want project, team or org)", s.Kind)
}

// KBDocument is one row of kb_documents.
type KBDocument struct {
	ID          int64
	ScopeKind   string
	ScopeID     int
	Path        string
	Title       string
	Tags        []string
	Author      string
	Reason      string
	Content     string
	Annotations []domain.KBAnnotation
	Version     int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
	DeletedBy   *string
}

// KBDocWrite is a create-or-update of one doc.
type KBDocWrite struct {
	Path    string
	Title   string
	Tags    []string
	Author  string
	Reason  string
	Content string
	// By is who made the change, recorded on the version row.
	By string
	// ExpectVersion, when > 0, makes the write fail with ErrConflict unless
	// the stored version still equals it (compare-and-set against a
	// concurrent writer). 0 writes unconditionally.
	ExpectVersion int
}

const kbDocCols = `id, scope_kind, scope_id, path, title, tags, author, reason, content,
	annotations, version, created_at, updated_at, deleted_at, deleted_by`

func scanKBDocument(row pgx.Row, d *KBDocument) error {
	return row.Scan(&d.ID, &d.ScopeKind, &d.ScopeID, &d.Path, &d.Title, &d.Tags, &d.Author, &d.Reason, &d.Content,
		&d.Annotations, &d.Version, &d.CreatedAt, &d.UpdatedAt, &d.DeletedAt, &d.DeletedBy)
}

func kbTags(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

func kbAnnotations(a []domain.KBAnnotation) []domain.KBAnnotation {
	if a == nil {
		return []domain.KBAnnotation{}
	}
	return a
}

// lockKBDocument loads a doc, live or deleted, and locks its row until tx ends.
func lockKBDocument(ctx context.Context, tx pgx.Tx, scope KBScope, path string) (*KBDocument, error) {
	d := &KBDocument{}
	err := scanKBDocument(tx.QueryRow(ctx, `SELECT `+kbDocCols+` FROM kb_documents
		WHERE scope_kind = $1 AND scope_id = $2 AND path = $3 FOR UPDATE`, scope.Kind, scope.ID, path), d)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("kb doc %s in %s: %w", path, scope, ErrNotFound)
	}
	return d, err
}

func insertKBVersion(ctx context.Context, tx pgx.Tx, d *KBDocument, change, by string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO kb_document_versions (document_id, version, change, title, tags, content, changed_by, changed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, d.ID, d.Version, change, d.Title, kbTags(d.Tags), d.Content, by, at)
	return err
}

// UpsertKBDocument creates a doc or updates it in one transaction. A version
// row is written when the doc is created, revived from a soft delete, or its
// content, title or tags change, and only those writes move updated_at. A
// change to only author or reason updates the row without a version and
// keeps updated_at, so metadata touches don't make an old doc look new; an
// identical write changes nothing. Reviving a deleted doc clears its
// annotations: they were notes on the deleted doc, not on the new one. changed reports whether the row was
// written at all.
func (r *Repo) UpsertKBDocument(ctx context.Context, scope KBScope, w KBDocWrite) (doc *KBDocument, changed bool, err error) {
	if err := scope.validate(); err != nil {
		return nil, false, err
	}
	tags := kbTags(w.Tags)
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)

	d, err := lockKBDocument(ctx, tx, scope, w.Path)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	if d == nil {
		if w.ExpectVersion > 0 {
			return nil, false, &conflictError{fmt.Sprintf("kb doc %s in %s does not exist (expected version %d)", w.Path, scope, w.ExpectVersion)}
		}
		d = &KBDocument{}
		err := scanKBDocument(tx.QueryRow(ctx, `INSERT INTO kb_documents (scope_kind, scope_id, path, title, tags, author, reason, content, version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1)
			ON CONFLICT (scope_kind, scope_id, path) DO NOTHING
			RETURNING `+kbDocCols, scope.Kind, scope.ID, w.Path, w.Title, tags, w.Author, w.Reason, w.Content), d)
		switch {
		case err == nil:
			if err := insertKBVersion(ctx, tx, d, kbChangeCreate, w.By, d.UpdatedAt); err != nil {
				return nil, false, err
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, false, err
			}
			return d, true, nil
		case err != pgx.ErrNoRows:
			return nil, false, err
		}
		// Another writer created it between our SELECT and INSERT: update it.
		if d, err = lockKBDocument(ctx, tx, scope, w.Path); err != nil {
			return nil, false, err
		}
	}

	if w.ExpectVersion > 0 && d.Version != w.ExpectVersion {
		return nil, false, &conflictError{fmt.Sprintf("kb doc %s in %s is at version %d, not %d", w.Path, scope, d.Version, w.ExpectVersion)}
	}
	revive := d.DeletedAt != nil
	versioned := revive || d.Content != w.Content || d.Title != w.Title || !slices.Equal(kbTags(d.Tags), tags)
	if !versioned && d.Author == w.Author && d.Reason == w.Reason {
		return d, false, nil
	}
	next := d.Version
	if versioned {
		next++
	}
	updated := &KBDocument{}
	if err := scanKBDocument(tx.QueryRow(ctx, `UPDATE kb_documents SET title = $2, tags = $3, author = $4, reason = $5,
		content = $6, version = $7, updated_at = CASE WHEN $8 THEN NOW() ELSE updated_at END,
		annotations = CASE WHEN $9 THEN '[]'::jsonb ELSE annotations END,
		deleted_at = NULL, deleted_by = NULL
		WHERE id = $1 RETURNING `+kbDocCols, d.ID, w.Title, tags, w.Author, w.Reason, w.Content, next, versioned, revive), updated); err != nil {
		return nil, false, err
	}
	if versioned {
		change := kbChangeUpdate
		if revive {
			change = kbChangeCreate
		}
		if err := insertKBVersion(ctx, tx, updated, change, w.By, updated.UpdatedAt); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

// GetKBDocument loads a live (not soft-deleted) doc by path.
func (r *Repo) GetKBDocument(ctx context.Context, scope KBScope, path string) (*KBDocument, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	d := &KBDocument{}
	err := scanKBDocument(r.db.QueryRow(ctx, `SELECT `+kbDocCols+` FROM kb_documents
		WHERE scope_kind = $1 AND scope_id = $2 AND path = $3 AND deleted_at IS NULL`, scope.Kind, scope.ID, path), d)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("kb doc %s in %s: %w", path, scope, ErrNotFound)
	}
	return d, err
}

// ListKBDocuments lists a KB's live docs, most recently updated first.
// limit <= 0 means no limit.
func (r *Repo) ListKBDocuments(ctx context.Context, scope KBScope, limit, offset int) ([]KBDocument, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	q := `SELECT ` + kbDocCols + ` FROM kb_documents
		WHERE scope_kind = $1 AND scope_id = $2 AND deleted_at IS NULL
		ORDER BY updated_at DESC, id DESC OFFSET $3`
	args := []interface{}{scope.Kind, scope.ID, max(offset, 0)}
	if limit > 0 {
		q += ` LIMIT $4`
		args = append(args, limit)
	}
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KBDocument{}
	for rows.Next() {
		var d KBDocument
		if err := scanKBDocument(rows, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountKBDocuments counts a KB's live docs.
func (r *Repo) CountKBDocuments(ctx context.Context, scope KBScope) (int, error) {
	if err := scope.validate(); err != nil {
		return 0, err
	}
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM kb_documents
		WHERE scope_kind = $1 AND scope_id = $2 AND deleted_at IS NULL`, scope.Kind, scope.ID).Scan(&n)
	return n, err
}

// ListDeletedKBPaths lists the paths of a KB's soft-deleted docs (in the
// trash, not yet purged) that have no live row.
func (r *Repo) ListDeletedKBPaths(ctx context.Context, scope KBScope) ([]string, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx, `SELECT path FROM kb_documents
		WHERE scope_kind = $1 AND scope_id = $2 AND deleted_at IS NOT NULL ORDER BY path`, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteKBDocument soft-deletes a live doc and records a 'delete' version.
// A doc that is missing or already deleted is ErrNotFound.
func (r *Repo) DeleteKBDocument(ctx context.Context, scope KBScope, path, by string) error {
	if err := scope.validate(); err != nil {
		return err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	d, err := lockKBDocument(ctx, tx, scope, path)
	if err != nil {
		return err
	}
	if d.DeletedAt != nil {
		return fmt.Errorf("kb doc %s in %s: %w", path, scope, ErrNotFound)
	}
	var at time.Time
	if err := tx.QueryRow(ctx, `UPDATE kb_documents SET deleted_at = NOW(), deleted_by = $2, version = version + 1
		WHERE id = $1 RETURNING version, deleted_at`, d.ID, by).Scan(&d.Version, &at); err != nil {
		return err
	}
	if err := insertKBVersion(ctx, tx, d, kbChangeDelete, by, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RestoreKBDocument brings back a soft-deleted doc and records a 'restore'
// version. A doc that is missing or not deleted is ErrNotFound.
func (r *Repo) RestoreKBDocument(ctx context.Context, scope KBScope, path, by string) (*KBDocument, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	d, err := lockKBDocument(ctx, tx, scope, path)
	if err != nil {
		return nil, err
	}
	if d.DeletedAt == nil {
		return nil, fmt.Errorf("deleted kb doc %s in %s: %w", path, scope, ErrNotFound)
	}
	restored := &KBDocument{}
	if err := scanKBDocument(tx.QueryRow(ctx, `UPDATE kb_documents SET deleted_at = NULL, deleted_by = NULL,
		version = version + 1, updated_at = NOW() WHERE id = $1 RETURNING `+kbDocCols, d.ID), restored); err != nil {
		return nil, err
	}
	if err := insertKBVersion(ctx, tx, restored, kbChangeRestore, by, restored.UpdatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return restored, nil
}

// UpdateKBAnnotations changes a live doc's annotations: it locks the row
// (SELECT ... FOR UPDATE), passes the current annotations to fn and stores
// what fn returns, all in one transaction, so concurrent annotators can't
// overwrite each other. An error from fn aborts without writing. Annotation
// changes don't create a version and don't move updated_at.
func (r *Repo) UpdateKBAnnotations(ctx context.Context, scope KBScope, path string,
	fn func([]domain.KBAnnotation) ([]domain.KBAnnotation, error)) (*KBDocument, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	d, err := lockKBDocument(ctx, tx, scope, path)
	if err != nil {
		return nil, err
	}
	if d.DeletedAt != nil {
		return nil, fmt.Errorf("kb doc %s in %s: %w", path, scope, ErrNotFound)
	}
	next, err := fn(slices.Clone(kbAnnotations(d.Annotations)))
	if err != nil {
		return nil, err
	}
	next = kbAnnotations(next)
	if _, err := tx.Exec(ctx, `UPDATE kb_documents SET annotations = $2 WHERE id = $1`, d.ID, next); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	d.Annotations = next
	return d, nil
}

// SaveKBAnnotations replaces a live doc's annotations (under the same row
// lock as UpdateKBAnnotations).
func (r *Repo) SaveKBAnnotations(ctx context.Context, scope KBScope, path string, annotations []domain.KBAnnotation) (*KBDocument, error) {
	return r.UpdateKBAnnotations(ctx, scope, path, func([]domain.KBAnnotation) ([]domain.KBAnnotation, error) {
		return annotations, nil
	})
}

// InsertKBDocumentIfMissing copies a doc in for the backfill from Typesense.
// It writes only when no row exists for the path, live or deleted (a deleted
// row means the doc was removed after Postgres took over, so it must not come
// back). It keeps d's CreatedAt and UpdatedAt (NOW() when zero), Author,
// Reason and Annotations, and records version 1 as a 'backfill' change.
// inserted reports whether the row was written.
func (r *Repo) InsertKBDocumentIfMissing(ctx context.Context, scope KBScope, d KBDocument, by string) (inserted bool, err error) {
	if err := scope.validate(); err != nil {
		return false, err
	}
	updatedAt := d.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	createdAt := d.CreatedAt
	if createdAt.IsZero() {
		createdAt = updatedAt
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	row := &KBDocument{}
	err = scanKBDocument(tx.QueryRow(ctx, `INSERT INTO kb_documents (scope_kind, scope_id, path, title, tags, author, reason,
		content, annotations, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, $10, $11)
		ON CONFLICT (scope_kind, scope_id, path) DO NOTHING
		RETURNING `+kbDocCols, scope.Kind, scope.ID, d.Path, d.Title, kbTags(d.Tags), d.Author, d.Reason,
		d.Content, kbAnnotations(d.Annotations), createdAt, updatedAt), row)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := insertKBVersion(ctx, tx, row, kbChangeBackfill, by, updatedAt); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// StreamKBDocuments calls fn with each live doc of a KB, in id order, without
// holding them all in memory (for reindexing Typesense). An error from fn
// stops the stream and is returned.
func (r *Repo) StreamKBDocuments(ctx context.Context, scope KBScope, fn func(KBDocument) error) error {
	if err := scope.validate(); err != nil {
		return err
	}
	rows, err := r.db.Query(ctx, `SELECT `+kbDocCols+` FROM kb_documents
		WHERE scope_kind = $1 AND scope_id = $2 AND deleted_at IS NULL ORDER BY id`, scope.Kind, scope.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var d KBDocument
		if err := scanKBDocument(rows, &d); err != nil {
			return err
		}
		if err := fn(d); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ---- Backfill tracking (kb_backfills) ----

// KBBackfill is one KB's backfill state.
type KBBackfill struct {
	ScopeKind   string
	ScopeID     int
	StartedAt   time.Time
	CompletedAt *time.Time
	DocCount    int
	LastError   string
}

// StartKBBackfill records that a KB's backfill has begun (again). It clears
// the last error but leaves an earlier completion in place.
func (r *Repo) StartKBBackfill(ctx context.Context, scope KBScope) error {
	if err := scope.validate(); err != nil {
		return err
	}
	_, err := r.db.Exec(ctx, `INSERT INTO kb_backfills (scope_kind, scope_id) VALUES ($1, $2)
		ON CONFLICT (scope_kind, scope_id) DO UPDATE SET started_at = NOW(), last_error = ''`, scope.Kind, scope.ID)
	return err
}

// CompleteKBBackfill marks a KB's backfill complete with the number of docs
// it copied. From then on Postgres holds every doc of that KB.
func (r *Repo) CompleteKBBackfill(ctx context.Context, scope KBScope, docCount int) error {
	if err := scope.validate(); err != nil {
		return err
	}
	_, err := r.db.Exec(ctx, `INSERT INTO kb_backfills (scope_kind, scope_id, completed_at, doc_count)
		VALUES ($1, $2, NOW(), $3)
		ON CONFLICT (scope_kind, scope_id) DO UPDATE SET completed_at = NOW(), doc_count = $3, last_error = ''`,
		scope.Kind, scope.ID, docCount)
	return err
}

// FailKBBackfill records why a KB's backfill stopped; it stays incomplete.
func (r *Repo) FailKBBackfill(ctx context.Context, scope KBScope, msg string) error {
	if err := scope.validate(); err != nil {
		return err
	}
	_, err := r.db.Exec(ctx, `INSERT INTO kb_backfills (scope_kind, scope_id, last_error) VALUES ($1, $2, $3)
		ON CONFLICT (scope_kind, scope_id) DO UPDATE SET last_error = $3`, scope.Kind, scope.ID, msg)
	return err
}

// IsKBBackfillComplete reports whether a KB's backfill has completed.
func (r *Repo) IsKBBackfillComplete(ctx context.Context, scope KBScope) (bool, error) {
	if err := scope.validate(); err != nil {
		return false, err
	}
	var done bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kb_backfills
		WHERE scope_kind = $1 AND scope_id = $2 AND completed_at IS NOT NULL)`, scope.Kind, scope.ID).Scan(&done)
	return done, err
}

// ListBackfilledKBScopes lists every KB whose backfill has completed, the
// KBs whose docs are all in Postgres (the reconciler works only on those).
func (r *Repo) ListBackfilledKBScopes(ctx context.Context) ([]KBScope, error) {
	rows, err := r.db.Query(ctx, `SELECT scope_kind, scope_id FROM kb_backfills
		WHERE completed_at IS NOT NULL ORDER BY scope_kind, scope_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KBScope
	for rows.Next() {
		var sc KBScope
		if err := rows.Scan(&sc.Kind, &sc.ID); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// GetKBBackfill loads a KB's backfill state; ErrNotFound if it never started.
func (r *Repo) GetKBBackfill(ctx context.Context, scope KBScope) (*KBBackfill, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	b := &KBBackfill{}
	err := r.db.QueryRow(ctx, `SELECT scope_kind, scope_id, started_at, completed_at, doc_count, last_error
		FROM kb_backfills WHERE scope_kind = $1 AND scope_id = $2`, scope.Kind, scope.ID).Scan(
		&b.ScopeKind, &b.ScopeID, &b.StartedAt, &b.CompletedAt, &b.DocCount, &b.LastError)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("kb backfill for %s: %w", scope, ErrNotFound)
	}
	return b, err
}
