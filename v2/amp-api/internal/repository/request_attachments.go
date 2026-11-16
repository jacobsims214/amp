package repository

// Team requests: the private attachment store — metadata and bytes, tenant-scoped,
// with the request's count and total bounds serialised on the request's row lock
// (epic 202, story 548, AMP #3955; approved product contract artifact 36 v2 §1 and
// decision D3; build slice G2 of
// discoveries/team-inbox-approved-build-slices.md).
//
// Migration 056 (request_attachments) is the table and domain/request_attachment.go
// is the record, the bounds, the scan vocabulary and the derived hold. This file is
// the only write and read path to the rows, and it implements exactly the five
// things the schema left to Go (see 056's header, "WHAT IS NOT ENFORCED HERE, BY
// DESIGN"):
//
//  1. The count and total bounds are properties of the REQUEST, not of one row, so a
//     CHECK cannot see them. InsertRequestAttachment locks the request row
//     (request_claims.go's lockRequest, through the shared request contract), reads
//     the sizes already stored under that lock, and applies the exact set the domain
//     owns — domain.ValidateRequestAttachmentSet over existing + new. Two uploads at
//     once therefore serialise on the request: the second one counts the first one's
//     row and is refused, instead of both counting four and both storing a fifth and
//     a sixth. Nothing is written when the bound refuses, so a request can never hold
//     six files or 61 MiB because of a race.
//
//  2. Size is the bytes' size. There is deliberately no field on the insert for a
//     caller-declared length: SizeBytes is len(Content), the length that actually
//     arrived. The classic upload bug (storing the client's Content-Length) is not
//     merely checked here, it is unexpressible.
//
//  3. The digest is the bytes' digest. The repository computes sha256 over the exact
//     bytes it inserts. A caller may pass the digest it also computed (the service
//     does, and it is what makes "these bytes are the file I validated" checkable),
//     and a disagreement is refused rather than stored: a row whose sha256 does not
//     match its content is a download ETag that lies. 056 does not re-hash on every
//     UPDATE for cost reasons; this is the one writer that has both halves.
//
//  4. The stored content type is the SNIFFED type, never the client's. This layer does
//     not sniff: it has no net/http, no mime, no io (TestRequestAttachmentStorage-
//     LayerStaysMetadataAndBytes asserts the import list). The service (G2) sniffs
//     once and passes the type; domain.RequestAttachment.Validate then refuses a type
//     a sniffer could not have produced and drops any parameter, which is how a client
//     Content-Type header cannot reach the column.
//
//  5. Tenancy is part of every statement, not a filter a caller remembers. A request
//     belongs to one org and one team (053's composite key), so every read here joins
//     team_requests on the request id and matches BOTH halves, and every write first
//     locks the request through the shared tenant predicate. An attachment id in
//     another team — including another team of the same org — is answered exactly like
//     an id that does not exist (ErrNotFound, the request surface's one 404), so a
//     foreign attachment's bytes cannot be read and its existence cannot be probed.
//
// What this file deliberately does NOT do, and where each belongs:
//
//   - Sniffing (net/http.DetectContentType) is the upload service's (G2), as for
//     artifacts: internal/service/artifacts.go's artifactContentType is the existing
//     scanner and it is used unchanged.
//   - Serving. There is no HTTP handler, no signed URL, no public URL and no
//     disposition header here: 056 has no locator column and nothing in this file
//     returns one. LoadRequestAttachment is the only path to bytes and it returns the
//     whole record with them, so the one download gate (G3) decides on the same row it
//     is about to serve — domain.RequestAttachmentDownloadRefusal — instead of on a
//     metadata copy that could disagree. Refusing here would hide which refusal to
//     report and would need a clock inside the storage layer.
//   - Scanning. AMP ships the interface, not a service, so nothing here invents a
//     verdict: an empty scan state is stored as 'unsupported' (the domain's default,
//     the schema's default), and this file never names RequestScanClean at all — a
//     storage layer that can write "clean" is a storage layer that can claim a scan it
//     never ran. TestRequestAttachmentStorageLayerStaysMetadataAndBytes fails if that
//     word appears here.
//   - Retention. expires_at / keep / kept_by / kept_at are preserved exactly as the
//     caller passes them (the lifecycle service stamps expiry when a request closes,
//     D3); the purge job (G4) is not in this file.
//   - Authorization. Who may upload, download, keep or release is the service's and
//     authz's decision. This layer records the actor (uploaded_by) and refuses nothing
//     it cannot see.
//   - Lifecycle and history. No request_events row is appended here: an attachment is
//     not a transition, and the service that records the act owns its audit and
//     history context (the claim file's rule).
//
// No schema change, no migration, no new index: 056's three-index proof stays true.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// ErrRequestAttachmentBounds marks an upload refused because its request already
// carries the most files, or the most bytes, the contract allows. It is an invalid
// request (the upload service answers `request.invalid`, 400), never a conflict: the
// same bytes will not fit on a retry, so a 409 would tell a client to try again
// forever. Nothing was written.
var ErrRequestAttachmentBounds = errors.New("a request's attachments are bounded by count and total size: send fewer files, or send the rest in a second request")

// ErrRequestAttachmentDigest marks an insert whose caller-supplied sha256 does not
// match the bytes it supplied. Like ErrRequestTenantScope it reports a caller bug
// rather than a refusal worth showing a requester: the two values are about the same
// bytes, so a disagreement means the service validated a different payload from the
// one it is storing — and storing it would produce a download ETag that lies about
// the content, which is exactly the row 056 declines to create.
var ErrRequestAttachmentDigest = errors.New("sha256 is the digest of the bytes being stored: recompute it from the content you are inserting")

// RequestAttachmentBounds is an over-count or over-total upload, with the numbers
// that were true under the request's lock. It matches ErrRequestAttachmentBounds so a
// caller can branch without a type assertion, and it carries what was there when the
// refusal was decided because that is what the message has to name ("5 files, 58 MiB;
// this file would make 6").
type RequestAttachmentBounds struct {
	RequestID  int
	Existing   int   // files the request already had, read under the lock
	TotalBytes int64 // bytes they already held
	Err        error // the domain's refusal, which names the bound that was hit
}

func (e *RequestAttachmentBounds) Error() string {
	return fmt.Sprintf("request %d already carries %d attachments totalling %d bytes: %v", e.RequestID, e.Existing, e.TotalBytes, e.Err)
}

func (e *RequestAttachmentBounds) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrRequestAttachmentBounds) true for a refused upload.
func (e *RequestAttachmentBounds) Is(target error) bool { return target == ErrRequestAttachmentBounds }

// requestAttachmentCols is the column list of domain.RequestAttachment, in migration
// 056's order. One list and one scanner pair, for the reason requestCols exists: a
// column added to request_attachments and forgotten here would read as a zero value
// rather than as an error. TestRequestAttachmentColumnsMatchSchema compares it to the
// live table.
//
// It takes an alias because the two reads in this file join team_requests for the
// tenant, and BOTH tables have `id` and `created_at`: an unqualified list in a join is
// ambiguous, and postgres refuses it. The alias is applied here, once, so no read can
// forget it (the requestCommentCols(alias) pattern). An empty alias is the
// unqualified form the INSERT's RETURNING uses, where there is no join to disambiguate
// from.
func requestAttachmentCols(alias string) string {
	return joinAttachmentCols(alias, []string{
		"id", "request_id", "file_name", "content_type", "size_bytes", "sha256",
		"scan_state", "uploaded_by", "created_at", "keep", "kept_by", "kept_at", "expires_at",
		"released_at", "released_by", "content",
	})
}

// requestAttachmentMetaCols is requestAttachmentCols without the bytes. Every read
// that is not a download uses this one: a panel, a list or an event carries metadata,
// and loading up to 25 MiB per row to render a filename is how a queue screen starts
// allocating a customer's attachments. The test asserts the meta list is exactly the
// full list minus `content`, so neither can drift on its own.
func requestAttachmentMetaCols(alias string) string {
	return joinAttachmentCols(alias, []string{
		"id", "request_id", "file_name", "content_type", "size_bytes", "sha256",
		"scan_state", "uploaded_by", "created_at", "keep", "kept_by", "kept_at", "expires_at",
		"released_at", "released_by",
	})
}

// joinAttachmentCols prefixes every column with alias (when there is one) and joins
// them into a select list.
func joinAttachmentCols(alias string, cols []string) string {
	if alias == "" {
		return strings.Join(cols, ", ")
	}
	qualified := make([]string, len(cols))
	for i, c := range cols {
		qualified[i] = alias + "." + c
	}
	return strings.Join(qualified, ", ")
}

// requestAttachmentMetaFields returns the scan targets for requestAttachmentMetaCols,
// in order. A function rather than an inline list so the metadata read, the byte read
// and the insert's RETURNING cannot disagree about the order: the bytes read appends
// its one extra target below.
func requestAttachmentMetaFields(a *domain.RequestAttachment) []any {
	return []any{
		&a.ID, &a.RequestID, &a.FileName, &a.ContentType, &a.SizeBytes, &a.SHA256,
		&a.ScanState, &a.UploadedBy, &a.CreatedAt,
		&a.Keep, &a.KeptBy, &a.KeptAt, &a.ExpiresAt,
		&a.ReleasedAt, &a.ReleasedBy,
	}
}

// scanRequestAttachmentMeta scans one metadata row and leaves Content nil, so "no
// bytes were read" is a fact of the value rather than an assumption the caller makes.
func scanRequestAttachmentMeta(row rowScanner, a *domain.RequestAttachment) error {
	if err := row.Scan(requestAttachmentMetaFields(a)...); err != nil {
		return err
	}
	a.Content = nil
	return nil
}

// scanRequestAttachment scans one full row, bytes included.
func scanRequestAttachment(row rowScanner, a *domain.RequestAttachment) error {
	targets := append(requestAttachmentMetaFields(a), &a.Content)
	if err := row.Scan(targets...); err != nil {
		return err
	}
	return nil
}

// requestAttachmentTenantWhere is the tenant predicate for a read that joins its
// request, with the org and team taking the two placeholders after base. Both halves
// are matched, because an attachment belongs to exactly one org and one team through
// its request (053's composite key): matching the org alone would let one team of an
// org read another team's private files, which is the one thing an attachment must
// never allow.
//
// The condition is on the REQUEST's tenant and never on the attachment, which carries
// no tenant columns of its own — the request is the record that has them, so a copy on
// the attachment could disagree with it and become the weaker check.
//
// It is a function of the placeholder base because the two reads that use it number
// their arguments differently (the list has no attachment id, the single read does),
// and a postgres statement cannot skip a placeholder: it refuses `$2` when only $1,
// $3 and $4 are referenced. Taking the base keeps ONE predicate, written once, that
// both reads splice in — the requestCommentCols(alias) pattern.
func requestAttachmentTenantWhere(base int) string {
	return fmt.Sprintf(` r.id = a.request_id AND r.org_id = $%d AND r.team_id = $%d`, base, base+1)
}

// RequestAttachmentInsert is one private upload: which request in which tenant, the
// bytes, the type the service sniffed, and the columns it may stamp.
//
// There is deliberately no SizeBytes and no required SHA256. SizeBytes is the length
// of Content (see the header); SHA256 is computed from Content, and when the caller
// also passes one it is verified rather than trusted. That leaves no way to describe
// the bytes as something other than what they are.
type RequestAttachmentInsert struct {
	// The request, and the tenant it must belong to. Both halves are required: a write
	// scoped by only one of them would be a cross-team (or cross-org) write primitive.
	RequestID int
	OrgID     int
	TeamID    int

	// FileName is the requester's display name. domain.NormalizeRequestAttachmentFileName
	// refuses a path, a control character and an over-long name; it never decides the
	// content type (at most it can escalate a hold).
	FileName string

	// ContentType is the type the upload service SNIFFED from Content, never the
	// client's Content-Type header. Validated against the domain's allowlist of types a
	// sniffer really produces, so a client-declared application/json or image/svg+xml
	// cannot be stored.
	ContentType string

	// Content is the file. Its length is the stored size and its digest is the stored
	// sha256.
	Content []byte

	// SHA256, when set, is the digest the caller computed over Content. It is checked,
	// not trusted: a disagreement is ErrRequestAttachmentDigest and nothing is written.
	SHA256 string

	// ScanState is the verdict a scanner reached, when one has. Empty means none, and
	// lands on 'unsupported' ("no verdict") — never on 'clean'.
	ScanState domain.RequestScanState

	// UploadedBy is the AMP user who uploaded the file, when there is one. A connector
	// or MCP service client may have no AMP user; deleting the user later nulls it
	// without deleting the file (056's ON DELETE SET NULL).
	UploadedBy *int

	// The retention columns (D3), preserved as given: the lifecycle service stamps
	// expires_at when a request closes and a keep pins a file, and the purge job (G4)
	// reads them. This layer never moves that clock.
	Keep      bool
	KeptBy    *int
	KeptAt    *time.Time
	ExpiresAt *time.Time

	// The restricted-file hold. A team lead's release is recorded here by the service
	// that authorized it; domain.RequestAttachment.Validate refuses a release on a file
	// that was never held (a restricted type or a code-like name), and 056's CHECK is
	// the same rule in SQL.
	ReleasedAt *time.Time
	ReleasedBy *int
}

// RequestAttachmentRef names one stored attachment, and the request and tenant it must
// belong to. It is the caller's part of a byte read or a delete: an attachment id alone
// is never enough, because an id-only read is the cross-tenant read primitive this file
// exists to not have.
type RequestAttachmentRef struct {
	AttachmentID int64
	RequestID    int
	OrgID        int
	TeamID       int
}

// InsertRequestAttachment stores one attachment's metadata and bytes, and is the only
// insert path to request_attachments.
//
// The request's count and total bounds are enforced under the request's row lock: the
// sizes already stored are read inside the same transaction, the new file is added to
// them, and domain.ValidateRequestAttachmentSet decides. A race therefore cannot
// produce a sixth file or an over-total request — the loser blocks until the winner
// commits, then counts the winner's row and is refused with *RequestAttachmentBounds.
//
// The stored record is returned with Content nil (metadata only, as every read of a
// list or a panel gets it); the bytes are what the caller just passed, and
// LoadRequestAttachment is how they are read back. A foreign or unknown request is
// ErrNotFound and writes nothing, exactly as it is everywhere else in the package. A
// departed uploader (a user row deleted between the service's check and this write) is
// an ordinary not-found for that user rather than a constraint error escaping as a 500.
func (r *Repo) InsertRequestAttachment(ctx context.Context, in RequestAttachmentInsert) (*domain.RequestAttachment, error) {
	if err := checkRequestWriteScope(in.RequestID, in.OrgID, in.TeamID); err != nil {
		return nil, err
	}
	a, err := attachmentRecord(in)
	if err != nil {
		return nil, err
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Lock the request first: the count and total are read below and have to still be
	// true when the row is inserted. Every attachment write for one request serialises
	// here, which is what makes the bounds a guarantee instead of a race.
	if _, err := lockRequest(ctx, tx, RequestClaimWrite{RequestID: in.RequestID, OrgID: in.OrgID, TeamID: in.TeamID}); err != nil {
		return nil, err
	}

	existing, total, err := requestAttachmentSizes(ctx, tx, in.RequestID)
	if err != nil {
		return nil, err
	}
	// The bound check is the domain's, over the set as it will be after this insert. One
	// definition of the numbers (MaxRequestAttachments, MaxRequestAttachmentsTotalBytes)
	// means the repository, the service and a test cannot disagree about them.
	candidate := make([]domain.RequestAttachment, 0, len(existing)+1)
	for _, size := range existing {
		candidate = append(candidate, domain.RequestAttachment{SizeBytes: size})
	}
	candidate = append(candidate, domain.RequestAttachment{SizeBytes: a.SizeBytes})
	if err := domain.ValidateRequestAttachmentSet(candidate); err != nil {
		return nil, &RequestAttachmentBounds{RequestID: in.RequestID, Existing: len(existing), TotalBytes: total, Err: err}
	}

	err = scanRequestAttachmentMeta(tx.QueryRow(ctx, `INSERT INTO request_attachments
		(request_id, file_name, content_type, size_bytes, sha256, scan_state, uploaded_by,
		 keep, kept_by, kept_at, expires_at, released_at, released_by, content)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING `+requestAttachmentMetaCols(""),
		a.RequestID, a.FileName, a.ContentType, a.SizeBytes, a.SHA256, string(a.ScanState), a.UploadedBy,
		a.Keep, a.KeptBy, a.KeptAt, a.ExpiresAt, a.ReleasedAt, a.ReleasedBy, in.Content), a)
	if err != nil {
		return nil, requestAttachmentWriteError(err, in.UploadedBy, in.KeptBy, in.ReleasedBy)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// attachmentRecord builds and validates the row this insert would write. Everything is
// in-process: the size is the content's length, the digest is computed from the content
// (and checked against the caller's when it supplied one), and the record's own rules —
// a display name that is not a path, a sniffed type AMP stores, a known scan state, a
// coherent keep/release pair, and no release on a file that was never held — are
// domain.RequestAttachment.Validate's, so this path cannot accept what the domain would
// refuse nor store what 056's CHECKs would.
func attachmentRecord(in RequestAttachmentInsert) (*domain.RequestAttachment, error) {
	a := &domain.RequestAttachment{
		RequestID:   in.RequestID,
		FileName:    in.FileName,
		ContentType: in.ContentType,
		// The length that arrived. Never a declared one: 056's
		// request_attachments_size_matches_content would refuse a lie, and this line is
		// why the lie is never even expressible on this path.
		SizeBytes:  int64(len(in.Content)),
		SHA256:     in.SHA256,
		ScanState:  in.ScanState,
		UploadedBy: in.UploadedBy,
		Keep:       in.Keep,
		KeptBy:     in.KeptBy,
		KeptAt:     in.KeptAt,
		ExpiresAt:  in.ExpiresAt,
		ReleasedAt: in.ReleasedAt,
		ReleasedBy: in.ReleasedBy,
	}
	digest := sha256.Sum256(in.Content)
	computed := hex.EncodeToString(digest[:])
	if a.SHA256 == "" {
		a.SHA256 = computed
	}
	if err := a.Validate(); err != nil {
		return nil, fmt.Errorf("attachment %q on request %d: %w", in.FileName, in.RequestID, err)
	}
	// a.SHA256 is now the domain's normalized form (trimmed, lower-case), so an
	// upper-case digest a caller computed from the right bytes is accepted and stored
	// once, in the form the ETag uses.
	if a.SHA256 != computed {
		return nil, fmt.Errorf("attachment %q on request %d: the supplied sha256 %s is not the digest of the %d bytes supplied: %w",
			a.FileName, a.RequestID, a.SHA256, len(in.Content), ErrRequestAttachmentDigest)
	}
	return a, nil
}

// requestAttachmentSizes lists the size of every file the request already carries,
// under the caller's lock, plus their total. Sizes rather than whole rows: the bound
// check needs a number per file and nothing else, so a request holding 25 MiB files
// does not pull 125 MiB through the connection to count five rows.
func requestAttachmentSizes(ctx context.Context, q querier, requestID int) ([]int64, int64, error) {
	rows, err := q.Query(ctx, `SELECT size_bytes FROM request_attachments WHERE request_id = $1 ORDER BY id`, requestID)
	if err != nil {
		return nil, 0, fmt.Errorf("sizes of request %d's attachments: %w", requestID, err)
	}
	defer rows.Close()
	var sizes []int64
	var total int64
	for rows.Next() {
		var size int64
		if err := rows.Scan(&size); err != nil {
			return nil, 0, fmt.Errorf("scan a size of request %d's attachments: %w", requestID, err)
		}
		sizes = append(sizes, size)
		total += size
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("sizes of request %d's attachments: %w", requestID, err)
	}
	return sizes, total, nil
}

// requestAttachmentWriteError turns the one kind of foreign key an insert can violate
// into the ordinary not-found for that user. The request is locked before the insert and
// therefore known to exist in this tenant, so a 23503 here cannot be the request: it is
// uploaded_by, kept_by or released_by naming a users row that is gone (all three are
// ON DELETE SET NULL, so the reference is optional and an existing file survives without
// it — but a write cannot invent a user id).
//
// It names which column failed, because the three are different bugs to the caller: a
// departed uploader is an upload by someone who just deleted their account, while a
// departed kept_by is a keep attributed to the wrong person. Anything else is returned as
// itself so a real database failure keeps its identity.
func requestAttachmentWriteError(err error, uploadedBy, keptBy, releasedBy *int) error {
	if !isForeignKeyViolation(err) {
		return fmt.Errorf("insert attachment: %w", err)
	}
	for _, ref := range []struct {
		column string
		user   *int
	}{{"uploaded_by", uploadedBy}, {"kept_by", keptBy}, {"released_by", releasedBy}} {
		if ref.user != nil {
			return fmt.Errorf("attachment %s names user %d, who does not exist: %w", ref.column, *ref.user, ErrNotFound)
		}
	}
	return fmt.Errorf("insert attachment: %w", err)
}

// ListRequestAttachments returns one request's files in upload order (by id, not
// created_at, so rows written in one transaction keep a stable order — 056's index and
// the request_comments rule), metadata only: Content is nil on every returned value.
//
// There is no paging because there cannot be a long list: the request carries at most
// domain.MaxRequestAttachments (5) files, and 056's idx_request_attachments_request
// serves exactly this read. The tenant is required and enforced twice — the request is
// loaded through the tenant-scoped read (so a foreign request is ErrNotFound, not an
// empty list that a caller could mistake for "no files") and the query joins the
// request again and matches both halves.
func (r *Repo) ListRequestAttachments(ctx context.Context, requestID, orgID, teamID int) ([]domain.RequestAttachment, error) {
	if _, err := getRequest(ctx, r.db, requestID, orgID, teamID); err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx, `SELECT `+requestAttachmentMetaCols("a")+`
		  FROM request_attachments a
		  JOIN team_requests r ON`+requestAttachmentTenantWhere(2)+`
		 WHERE a.request_id = $1
		 ORDER BY a.id`, requestID, orgID, teamID)
	if err != nil {
		return nil, fmt.Errorf("list attachments of request %d: %w", requestID, err)
	}
	out, err := collect(rows, scanRequestAttachmentMeta)
	if err != nil {
		return nil, fmt.Errorf("list attachments of request %d: %w", requestID, err)
	}
	return out, nil
}

// GetRequestAttachment loads one attachment's metadata (Content nil) inside the request
// and tenant it belongs to. It is the read a panel uses to show a file's name, size,
// digest, scan state and hold; the bytes come from LoadRequestAttachment.
//
// A foreign attachment, an attachment of another request, and an unknown id are all
// ErrNotFound: the three are "not this request's attachment" and telling them apart
// would let an id walker probe another team's files.
func (r *Repo) GetRequestAttachment(ctx context.Context, ref RequestAttachmentRef) (*domain.RequestAttachment, error) {
	a, err := r.getRequestAttachment(ctx, ref, requestAttachmentMetaCols("a"), scanRequestAttachmentMeta)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// LoadRequestAttachment loads one attachment WITH its bytes, inside the request and
// tenant it belongs to, and is the only path to them in the whole package.
//
// It returns the whole record rather than a bare []byte on purpose: the download gate
// (G3) decides with domain.RequestAttachmentDownloadRefusal on the record's scan state,
// hold and expiry, and it must decide on the row it is about to serve. Handing back
// bytes alone would let a caller serve an infected or unreleased file because it only
// had the id. Nothing here decides whether the bytes may be served, and nothing here
// calls an unsupported file clean: the stored scan_state is returned exactly as it is.
func (r *Repo) LoadRequestAttachment(ctx context.Context, ref RequestAttachmentRef) (*domain.RequestAttachment, error) {
	a, err := r.getRequestAttachment(ctx, ref, requestAttachmentCols("a"), scanRequestAttachment)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// getRequestAttachment is the one statement behind both reads: the same join, the same
// tenant predicate, the same id and request pair; only the column list differs. Keeping
// it in one place is what makes "a download can never read a row a panel could not see"
// true by construction.
func (r *Repo) getRequestAttachment(ctx context.Context, ref RequestAttachmentRef, cols string, scan func(rowScanner, *domain.RequestAttachment) error) (*domain.RequestAttachment, error) {
	if ref.AttachmentID <= 0 {
		// The serial starts at 1, so a non-positive id cannot name a row: an ordinary
		// missing attachment, not a caller-scope bug.
		return nil, fmt.Errorf("attachment %d: %w", ref.AttachmentID, ErrNotFound)
	}
	if err := checkRequestWriteScope(ref.RequestID, ref.OrgID, ref.TeamID); err != nil {
		return nil, err
	}
	a := &domain.RequestAttachment{}
	err := scan(r.db.QueryRow(ctx, `SELECT `+cols+`
		  FROM request_attachments a
		  JOIN team_requests r ON`+requestAttachmentTenantWhere(3)+`
		 WHERE a.id = $1 AND a.request_id = $2`, ref.AttachmentID, ref.RequestID, ref.OrgID, ref.TeamID), a)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("attachment %d on request %d: %w", ref.AttachmentID, ref.RequestID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("load attachment %d on request %d: %w", ref.AttachmentID, ref.RequestID, err)
	}
	return a, nil
}

// DeleteRequestAttachment removes one attachment and, because the bytes live in the row
// (056), its bytes with it — no second store to leak behind and no orphan to reap.
//
// It answers changed=false when the row was already gone, which makes a retry after a
// lost response safe (the UnlinkRequest rule): an attachment that is already deleted is
// already deleted. It is scoped by the request's tenant through the request lock, so a
// foreign attachment id deletes nothing and answers false exactly as an id that never
// existed does — the caller cannot tell the two apart, which is the point.
//
// The request's row lock is taken even though no count is enforced here: the lock is
// what serialises every attachment write for a request, so a delete cannot slip between
// an upload's count and its insert and produce a request that is over the bound.
func (r *Repo) DeleteRequestAttachment(ctx context.Context, ref RequestAttachmentRef) (bool, error) {
	if err := checkRequestWriteScope(ref.RequestID, ref.OrgID, ref.TeamID); err != nil {
		return false, err
	}
	if ref.AttachmentID <= 0 {
		return false, fmt.Errorf("attachment %d: %w", ref.AttachmentID, ErrNotFound)
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// The lock also answers "is this request in this tenant?": a foreign or unknown
	// request is ErrNotFound before anything is deleted.
	if _, err := lockRequest(ctx, tx, RequestClaimWrite{RequestID: ref.RequestID, OrgID: ref.OrgID, TeamID: ref.TeamID}); err != nil {
		return false, err
	}
	var id int64
	err = tx.QueryRow(ctx, `DELETE FROM request_attachments
		WHERE id = $1 AND request_id = $2 RETURNING id`, ref.AttachmentID, ref.RequestID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("delete attachment %d on request %d: %w", ref.AttachmentID, ref.RequestID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
