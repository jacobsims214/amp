package service

// Team request uploads: the one authorized path that puts a requester's private
// file onto a request (epic 202, story 549, AMP #3961; approved product contract
// artifact 36 v2 §1 and decision D3; build slice G2 of
// discoveries/team-inbox-approved-build-slices.md, the sniffing half — the
// storage half is #3955's repository and the serving half is #3963).
//
// Three layers already exist and this file is the seam between them, so it does
// exactly the work neither of them can:
//
//   - domain/request_attachment.go owns the numbers (25 MiB per file, 5 files,
//     60 MiB per request), the closed set of stored content types, the scan
//     vocabulary, the derived hold and the retention rule. It deliberately
//     contains no bytes machinery and imports no sniffer.
//   - repository/request_attachments.go owns the rows: tenant-scoped writes, the
//     count and total bounds serialised on the request's row lock, size-is-the-
//     bytes and a verified digest. It deliberately does not sniff, and it never
//     names a scan verdict.
//   - this file answers the three questions left between them: MAY this caller
//     attach to this request, WHAT is this file (sniffed once, here, never the
//     client's word), and WHAT state is the stored row in (explicitly chosen,
//     never a claim AMP cannot back).
//
// # Who may upload (own/team)
//
// Two capabilities, and the requester's is not a junior team member's: the
// person who submitted the request may add a file to their own request, and a
// member of the inbox's team (which, per authz.InboxRole, includes the team's
// lead and an owner/admin of its org) may add one to any request in that inbox.
// The decision is RequestUploadCapabilityFor, a pure function, so the matrix is
// testable without a database; the rows behind it are authz.InboxRole's and the
// request's requester column, both re-read on every call — there is no cached
// allow-set, which is what makes losing a team membership take effect on the
// next upload (contract §2's per-request re-check).
//
// Everything else is refused as the request surface's single not-found. That
// includes the case that matters most: a caller from another team of the same
// org, another org entirely, or a requester who is not this request's requester
// learns nothing — not that the request exists, not that it has files, not how
// many. The 404 is the same bytes for an id that does not exist, which is the
// only way "foreign uploader denied" can also mean "existence never leaks"
// (slice B4).
//
// # What the file is: the sniffed type, and nothing else
//
// http.DetectContentType runs here, once, on the bytes that arrived, and the
// result is what the row stores. The input type has no content-type field at
// all, so a client's Content-Type header is not merely ignored — it is
// unexpressible on this path, the same trick the repository plays with the
// size. Two consequences worth knowing, because they surprise people:
//
//   - An HTML file named "screenshot.png" is stored as text/html, not image/png.
//     The name never decides the type; at most it can add a hold (below).
//   - A type the sniffer can produce but AMP does not store (video/mp4, a font,
//     an icon) is refused with request.invalid naming what is accepted. The
//     domain's allowlist is the set of types the sniffer really returns, so a
//     refusal here is "we cannot represent this file", never "the client lied".
//
// Sniffing here rather than in the repository is deliberate: the repository has
// no net/http import and a test asserts that, so there is exactly one place in
// the product where a stored content type is decided.
//
// # What state the row is in: explicit, and never "clean"
//
// AMP ships no malware scanner in v1 (the contract ships the interface, not the
// service), so an upload lands on domain.RequestScanUnsupported — "no verdict
// exists" — chosen here explicitly rather than left to a default. It is not
// pending either: pending means a scanner is queued and will answer, and with no
// scanner behind it that would hold every download forever (see
// domain.NormalizeRequestScanState, which makes the same argument for its
// default). It is emphatically not clean: clean is a claim that a scanner ran
// and found nothing, and nothing here can back that claim. A guard test parses
// this file and fails if it ever names the clean verdict.
//
// The restricted-file hold is DERIVED, not stored (056 keeps only the release
// columns, and a stored flag could disagree with the type it came from), so this
// path's job is the negative one: never write a release, and report the derived
// state honestly so a surface can say "held for a team lead" rather than
// "attached". Archives and executables — and an office file, which sniffs as a
// zip — are therefore stored and held, not refused: the approved mockup shows a
// customer sending an .exe that waits for a lead, and "ask for a different
// format" is the escape for the common case. A name can escalate a hold (a .sh
// file sniffs as plain text) and can never lift one.
//
// # What this file deliberately does not do
//
//   - No download permission. Bytes are uploaded here and served nowhere near
//     it: nothing in this file decides whether an attachment may be read, mints
//     a URL, sets a disposition header or calls domain's download refusal. The
//     ticket's rule is that uploading bytes and being allowed to read them are
//     separate decisions, and they are separate files (#3963 is the gate). The
//     returned record carries no bytes (the repository's insert returns metadata
//     only), so this method cannot be turned into a read path.
//   - No server-side fetch. There is no URL in the input, no redirect, no
//     "attach from a link": a column or an argument naming a place to fetch is
//     how an upload starts being an SSRF primitive (056's header says the same
//     for the table). The bytes arrive in memory or they do not arrive.
//   - No artifacts. An attachment is private and separate from an artifact
//     (contract §1), so nothing here reuses artifact storage, artifact
//     validation or artifact URLs — a test asserts the identifier never appears
//     in this file.
//   - No endpoints, no UI, no retention clock, no purge. Each is another
//     slice's (F5, G4); a keep or an expiry is preserved as given, and this path
//     writes neither.
//   - No lifecycle. An upload is not a transition: it appends no request_events
//     row, moves no state and does not bump rev. Whether a request's state
//     should refuse new files is a lifecycle rule, and the one transition policy
//     (#3953/#3960) owns it; inventing a state gate here would be a rule nobody
//     approved.
//   - No unscoped lookup. The caller must already hold the request's tenant
//     (org and team); this method never resolves a bare id, because a "find by
//     id alone" read is the cross-tenant enumeration primitive the request
//     surface exists without. The read path (#3958) is where an authorized
//     caller learns a request's tenant.
//   - No bytes-level logging. The audit row records ids, the sniffed type, the
//     size and the derived state — never the file name, the bytes or their
//     digest.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// requestUploadScanState is the scan state every upload lands on. It is a named
// constant, not an inline literal, because it is a product statement rather than
// an implementation detail: with no scanner in v1, "no verdict exists" is the
// only truthful state, and it must be the same one for every upload path.
// See the file header for why it is neither pending nor clean.
const requestUploadScanState = domain.RequestScanUnsupported

// RequestUploadCapability is who may attach a file to one request. The values
// are deliberately two, not a ladder: a requester adds to their own request and
// a team member adds to any request in the inbox, and neither is a weaker form
// of the other (a requester is not a junior team member and a team member is not
// the requester of a request they did not submit). Nothing downstream may widen
// one into the other.
type RequestUploadCapability int

const (
	// RequestUploadNone: no standing. Reported as the request surface's single
	// not-found, never as a 403 — a caller who may not upload must not learn
	// that the request exists.
	RequestUploadNone RequestUploadCapability = iota
	// RequestUploadRequester: the person the request was submitted by, and
	// nobody else.
	RequestUploadRequester
	// RequestUploadTeam: a member of the inbox's team (authz.InboxMember, which
	// includes its lead and an owner/admin of its org).
	RequestUploadTeam
)

func (c RequestUploadCapability) String() string {
	switch c {
	case RequestUploadRequester:
		return "requester"
	case RequestUploadTeam:
		return "team"
	}
	return "none"
}

// RequestUploadCapabilityFor decides, purely, whether p may attach a file to r
// given p's standing role on r's team.
//
// The team wins when a caller is both (a triager filing a request to their own
// inbox is still triaging it), and "their own" is the durable requester column,
// never an audit field: a request submitted by a connector or a service client
// has no requester user, so nobody can claim it as their own.
func RequestUploadCapabilityFor(r *domain.TeamRequest, p authz.Principal, role authz.InboxRole) RequestUploadCapability {
	if r == nil {
		// Nothing to attach to. Answering "team" for a nil request would let a
		// caller that forgot to load one upload against nothing.
		return RequestUploadNone
	}
	if role >= authz.InboxMember {
		return RequestUploadTeam
	}
	if p.HasUser() && r.RequesterUserID != nil && *r.RequesterUserID == p.UserID {
		return RequestUploadRequester
	}
	return RequestUploadNone
}

// AddRequestAttachmentInput is one upload: which request in which tenant, the
// requester's name for the file, and the bytes.
//
// There is deliberately no content-type field and no scan-state field. The type
// is sniffed from Content (so a client's header cannot reach the column), and
// the scan state is this file's decision (so no caller can claim a verdict).
// There is also no size field: the size is the length of Content, which the
// repository enforces against the bytes it stores.
type AddRequestAttachmentInput struct {
	// RequestID is the request the file belongs to. It is never enough on its
	// own — the tenant below is required — because an id-only write is the
	// cross-tenant primitive this surface does not have.
	RequestID int
	// OrgID and TeamID are the request's tenant, as resolved by an authorized
	// read. A wrong pair is an ordinary not-found; a missing one is a caller
	// bug, refused rather than guessed at.
	OrgID  int
	TeamID int

	// FileName is the requester's display name. It never decides the content
	// type; at most it can add a hold (domain.RequestAttachmentHeldByName).
	FileName string
	// Content is the file, as it arrived. Its length is the stored size and its
	// digest is the stored sha256.
	Content []byte
}

// AddRequestAttachment stores one private file on one request, and is the only
// upload path in the product.
//
// Order, and why: the cheap shape checks first (a name that is a path, an empty
// file, a file over the per-file bound) so a client's malformed upload is a 400
// naming the field and writes nothing; then the request is loaded and the caller
// authorized, so a stranger's payload is refused before this code looks inside
// it; then the type is sniffed and the digest computed, both from the exact
// bytes that will be stored; then the repository writes the row, where the
// request's count and total bounds are enforced under the request's row lock.
//
// A refusal never writes anything: a request that is over its count or total
// bound is refused by the repository's transaction before the insert, so a race
// cannot produce a sixth file or a 61st MiB.
//
// The returned record is metadata only (Content is nil), which is what makes
// "upload" and "download permission" separable: having uploaded a file tells a
// caller nothing about being allowed to read it back.
func (s *Service) AddRequestAttachment(ctx context.Context, p authz.Principal, in AddRequestAttachmentInput) (*domain.RequestAttachment, error) {
	// A submit-scoped credential may reach exactly one operation — the
	// submission itself, on its bound team (authz/token_scope.go) — and an
	// attachment on an existing request is not it. Refusing here is the
	// fail-closed direction, and it is the same answer the surface gate gives:
	// a submit-only token reaches no route but POST /api/teams/{id}/requests.
	// A connector's file riding along with its submission is the submit path's
	// decision, not a second operation granted to the scope.
	if err := p.RequireUnscoped("upload request attachments"); err != nil {
		return nil, err
	}

	// The name is validated first because it is free to check and the domain
	// owns the rule (one line, not a path, no control characters, bounded). A
	// name that is a path is refused here so no later layer has to think about
	// zip-slip or a Content-Disposition that breaks out of its header.
	name, err := domain.NormalizeRequestAttachmentFileName(in.FileName)
	if err != nil {
		return nil, authz.RequestInvalid("%s", err)
	}
	// An empty file has nothing to attach, and the domain refuses it too; the
	// refusal is worth making here because it costs nothing and names the field.
	if len(in.Content) == 0 {
		return nil, authz.RequestInvalid("content is empty: an attachment needs bytes, and an empty file has nothing to attach")
	}
	// The per-file bound, checked before the request is loaded and long before
	// the bytes reach Postgres. The request-level count and total bounds are the
	// repository's (they need the request's row lock); this one is a property of
	// the file alone, so it can be answered without a write attempt.
	if len(in.Content) > domain.MaxRequestAttachmentBytes {
		return nil, authz.RequestInvalid(
			"content is over the %d-byte per-file limit (got %d): send a smaller file, or ask the team for a different format",
			domain.MaxRequestAttachmentBytes, len(in.Content))
	}

	req, err := s.loadUploadRequest(ctx, in)
	if err != nil {
		return nil, err
	}
	role, err := s.Authz.InboxRole(ctx, p, req.TeamID)
	if err != nil {
		return nil, fmt.Errorf("resolve standing on team %d for an upload: %w", req.TeamID, err)
	}
	if RequestUploadCapabilityFor(req, p, role) == RequestUploadNone {
		// The one not-found. It names nothing — no id, no team, no state — so a
		// caller who may not upload cannot tell this request from one that does
		// not exist, which is what keeps "denied" from being an existence oracle.
		return nil, authz.RequestNotFound()
	}

	// Sniffed once, here, from the bytes that arrived. Normalizing through the
	// domain is what turns the sniffer's "text/plain; charset=utf-8" into the
	// stored "text/plain" and refuses a type AMP cannot represent.
	contentType, kind, err := domain.NormalizeRequestAttachmentContentType(http.DetectContentType(in.Content))
	if err != nil {
		return nil, authz.RequestInvalid("%s", err)
	}

	// The digest is computed from the same bytes the repository will store, and
	// passed so the repository can prove the two agree (ErrRequestAttachmentDigest
	// if they do not). It is the download's ETag; nothing here returns it to the
	// caller as an access right.
	digest := sha256.Sum256(in.Content)

	att, err := s.Repo.InsertRequestAttachment(ctx, repository.RequestAttachmentInsert{
		RequestID:   req.ID,
		OrgID:       req.OrgID,
		TeamID:      req.TeamID,
		FileName:    name,
		ContentType: contentType,
		Content:     in.Content,
		SHA256:      hex.EncodeToString(digest[:]),
		// Explicit, and the same for every upload: no scanner ran, so no verdict
		// exists. Never clean (a claim AMP cannot back) and never pending (a
		// queue with nothing behind it). See requestUploadScanState.
		ScanState: requestUploadScanState,
		// The uploader is the authenticated principal, never an input field, and
		// nil when the credential has no AMP user (the column is nullable for
		// exactly that reason).
		UploadedBy: p.UserRef(),
		// No keep and no release: an upload is not a retention decision and is
		// not a team lead's release of a held file. The hold is derived from the
		// type and the name, so leaving these nil is what makes a restricted
		// file arrive held rather than served.
	})
	if err != nil {
		return nil, requestUploadError(req, name, err)
	}

	s.recordAttachmentUpload(ctx, p, req, att, kind)
	return att, nil
}

// loadUploadRequest loads the request an upload names, inside the tenant the
// caller supplied.
//
// A request in another tenant and one that does not exist are the same answer
// (the surface's single not-found), because the repository's lookup already
// matches both halves of the tenant in one statement — there is no read-then-
// check window here for an existence probe to land in.
//
// A missing tenant is NOT a not-found: it is a caller that did not resolve the
// request through an authorized read, and answering 404 would hide that bug
// behind a legitimate refusal while telling the caller nothing. Every other
// failure is wrapped as itself, so a dead connection or a cancelled context is
// never reported as "no such request".
func (s *Service) loadUploadRequest(ctx context.Context, in AddRequestAttachmentInput) (*domain.TeamRequest, error) {
	req, err := s.Repo.GetRequest(ctx, in.RequestID, in.OrgID, in.TeamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, authz.RequestNotFound()
		}
		return nil, fmt.Errorf("load request %d for an upload: %w", in.RequestID, err)
	}
	return req, nil
}

// requestUploadError translates the repository's answers into the surface's
// stable codes.
//
//   - Over count or over total is request.invalid (400), not a conflict: the
//     same bytes will not fit on a retry, so a 409 would tell a client to try
//     again forever. The message carries the numbers that were true under the
//     request's lock, because "5 files, 58 MiB; this one would make 6" is what
//     the requester can act on.
//   - A not-found at write time means something this write names is gone: the
//     request was deleted between the load and the insert, or the caller's own
//     user row was (uploaded_by is the only foreign key a locked request leaves
//     able to fail). Both are the surface's one 404 rather than a 500 — the
//     request really is not there any more, and the caller's account being gone
//     is not a fact worth a different answer.
//   - A digest disagreement is a caller bug in this service (it computed the
//     digest from the same slice), so it is returned as itself and reaches the
//     log as a 500 rather than being dressed up as the requester's fault.
func requestUploadError(req *domain.TeamRequest, name string, err error) error {
	var bounds *repository.RequestAttachmentBounds
	if errors.As(err, &bounds) {
		return authz.RequestInvalid("%s", bounds.Error())
	}
	if errors.Is(err, repository.ErrNotFound) {
		return authz.RequestNotFound()
	}
	return fmt.Errorf("store attachment %q on request %d: %w", name, req.ID, err)
}

// recordAttachmentUpload writes the one audit row an upload gets: a
// `request.attachment.upload` entry on the request's org, targeting the request,
// with the stored facts and no content.
//
// It runs after the insert committed, and how it fails is audit.go's rule: the
// file is already stored, so a failure is logged rather than returned. A refused
// upload never reaches here, so the log counts files rather than attempts (the
// refusals themselves are recorded once as access.denied by the listener that
// served them, api/denied.go).
//
// What it records is the point: ids, the SNIFFED type, the size and the derived
// state. Not the file name (it is the requester's own text), not the bytes and
// not the digest — an audit row is read by people looking for who did what, and
// none of those three answers that. The scan state is written out so the log
// cannot later be read as evidence that a scan happened: it says unsupported,
// which is what the row says.
func (s *Service) recordAttachmentUpload(ctx context.Context, p authz.Principal, req *domain.TeamRequest, att *domain.RequestAttachment, kind domain.RequestAttachmentKind) {
	s.audit(ctx, p, domain.AuditEntry{
		Action:     "request.attachment.upload",
		OrgID:      intRef(req.OrgID),
		TargetType: "request",
		TargetID:   idStr(req.ID),
		TargetName: req.PublicRef,
		Summary:    fmt.Sprintf("attached a %s file (%d bytes) to request %s", att.ContentType, att.SizeBytes, req.PublicRef),
		Details: map[string]any{
			"request_id":    req.ID,
			"attachment_id": att.ID,
			"content_type":  att.ContentType,
			"kind":          string(kind),
			"size_bytes":    att.SizeBytes,
			"scan_state":    string(att.ScanState),
			// The derived hold, recorded because it is the state a surface acts
			// on ("waiting for a team lead") and because it must be readable in
			// the log without re-deriving it from the type and the name.
			"held": att.Held(),
		},
	})
}
