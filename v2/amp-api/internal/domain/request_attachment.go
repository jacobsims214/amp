package domain

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Private request attachments (epic 202, story 521; approved product contract
// artifact 36 v2 §1, build slices G1/G3/G4 of
// discoveries/team-inbox-approved-build-slices.md; numbers from the approved
// mockups: artifact 37 v1 "Per-file limit 25 MB, 5 files, 60 MB total" and
// artifact 38 v1 "25 MB each, 5 files").
//
// An attachment is a requester's private upload on a request: separate from an
// artifact, which is a reviewable deliverable. The contract says they are
// "private and separate from artifacts": bounded per file, count and total;
// the sniffed MIME type is stored, never the client's; scan_state gates
// download; served only as a short-lived signed URL, always as an attachment;
// kept while the request is active, then 90 days after close, then purged,
// with a team "keep" to pin them (D3).
//
// This file specifies that primitive — the record's metadata, the bounds, the
// scan states and the restricted-file hold — and nothing that touches bytes:
// no storage, no upload handler, no download path, no signed URL, no purge
// job, no migration, no UI. Three things are deliberately elsewhere:
//
//   - Sniffing. The upload path (G2) sniffs the bytes once and stores that
//     type. This file does not import net/http and never inspects content; it
//     only refuses a type a sniffer could not have produced, which is what
//     stops a client's Content-Type header from reaching the record.
//   - Scanning. AMP has no malware scanner (the contract ships the interface,
//     not a service), so nothing here claims a verdict it never got: see
//     RequestScanState, where unscanned is a state of its own.
//   - Authorization. Who may release a held file, and who may download it, is
//     the service's and authz's decision. This file records that a release
//     happened (ReleasedAt/ReleasedBy) and refuses to treat an unreleased
//     restricted file as servable; it does not decide who is a team lead.

// RequestScanState is whether malware scanning reached a verdict on an
// attachment's bytes. The contract requires downloads to be gated on this
// column and names these four values.
//
// The distinction that matters: clean is a claim, unsupported is the absence
// of one. AMP ships no scanning service in v1, so nothing may render an
// unsupported file as "scanned" or "clean" — that would be a security claim
// the product cannot back. Keeping them separate states is what makes the
// honest wording possible.
type RequestScanState string

const (
	// RequestScanPending: queued for a scan that has not produced a verdict.
	// Downloads are refused until it does.
	RequestScanPending RequestScanState = "pending"
	// RequestScanClean: a scanner ran and found nothing. The only state a
	// surface may call "scanned".
	RequestScanClean RequestScanState = "clean"
	// RequestScanInfected: a scanner found something. Refused and held; a
	// person decides what happens to the request.
	RequestScanInfected RequestScanState = "infected"
	// RequestScanUnsupported: no verdict exists because nothing scanned the
	// bytes — AMP has no scanning service. This is *unknown*, not clean. It is
	// also the default a new upload gets (see NormalizeRequestScanState), so a
	// write path that forgets to set a state can neither claim safety nor wedge
	// a file in pending behind a scanner that will never run.
	RequestScanUnsupported RequestScanState = "unsupported"
)

// RequestScanStates are the four states, in the order the contract lists them.
// The attachment table's scan_state CHECK uses exactly these values.
var RequestScanStates = []RequestScanState{
	RequestScanPending, RequestScanClean, RequestScanInfected, RequestScanUnsupported,
}

// SaysClean reports whether a surface may call the attachment scanned: only a
// real verdict does.
func (s RequestScanState) SaysClean() bool { return s == RequestScanClean }

// IsUnknown reports whether no scanner reached a verdict. Unknown is a
// distinct state from clean on purpose: one is the absence of a claim, the
// other is the claim, and treating either as the other is the mistake this
// type exists to prevent.
func (s RequestScanState) IsUnknown() bool { return s == RequestScanUnsupported }

// BlocksDownload reports whether the scan state alone refuses a download.
// Written as "not clean and not explicitly unsupported" rather than a list of
// bad values, so a value this file does not know fails closed instead of
// serving bytes.
func (s RequestScanState) BlocksDownload() bool {
	return !s.SaysClean() && !s.IsUnknown()
}

// NormalizeRequestScanState validates a scan state. An empty state is
// unsupported: with no scanning service in v1, "no verdict" is the only
// truthful default — clean would be a false claim, and pending would hold
// every download behind a scanner that never runs.
func NormalizeRequestScanState(s string) (RequestScanState, error) {
	st := RequestScanState(strings.ToLower(strings.TrimSpace(s)))
	if st == "" {
		return RequestScanUnsupported, nil
	}
	if !slices.Contains(RequestScanStates, st) {
		return "", fmt.Errorf("scan_state must be one of %s", requestNames(RequestScanStates))
	}
	return st, nil
}

// Bounds on private uploads. The contract (§1) requires per-file, count and
// total limits and the approved mockups state the numbers, so these constants
// are the product's bounds: the upload path validates against them and the UI
// names them. The house's byte constants are untyped ints in MiB (see
// MaxArtifactBytes), so "MB" here is MiB.
const (
	// MaxRequestAttachmentBytes caps one uploaded file (mockups: "25 MB
	// each").
	MaxRequestAttachmentBytes = 25 << 20

	// MaxRequestAttachments caps how many files one request may carry
	// (mockups: "5 files").
	MaxRequestAttachments = 5

	// MaxRequestAttachmentsTotalBytes caps one request's attachments
	// together (artifact 37's edge sheet: "60 MB total"). It is deliberately
	// far below MaxRequestAttachments*MaxRequestAttachmentBytes, so the total
	// still binds when every file is under the per-file limit: intake needs
	// both checks, never one standing in for the other.
	MaxRequestAttachmentsTotalBytes = 60 << 20

	// MaxRequestAttachmentNameLen caps the stored display name.
	MaxRequestAttachmentNameLen = 255
)

// RequestAttachmentRetentionDays is the contract's D3 window: an attachment is
// kept while its request is active, then 90 days after the request closes,
// then purged — unless a team member pins it with keep.
const RequestAttachmentRetentionDays = 90

// RequestAttachmentDisposition is the only Content-Disposition an attachment
// may be served with. The download path (G3) sets it, together with
// X-Content-Type-Options: nosniff; nothing serves an attachment inline, so an
// uploaded HTML or SVG file can never run with the viewer's session. It lives
// here so no surface re-invents the posture and a test can pin it.
const RequestAttachmentDisposition = "attachment"

// RequestAttachmentRetention is RequestAttachmentRetentionDays as a duration.
func RequestAttachmentRetention() time.Duration {
	return RequestAttachmentRetentionDays * 24 * time.Hour
}

// RequestAttachmentExpiresAt is when a closed request's attachments become
// purgeable, or nil when nothing may purge them: the request is still active
// (closedAt nil — D3's "kept while the request is active"), or a team member
// kept the file.
//
// The service stamps the returned value into expires_at when a request closes
// and clears it while the request is open; the purge job (G4) deletes rows
// whose expires_at has passed and that are not kept. The domain never moves
// that clock itself, which is why this is a pure function of the close time.
func RequestAttachmentExpiresAt(closedAt *time.Time, keep bool) *time.Time {
	if keep || closedAt == nil {
		return nil
	}
	at := closedAt.Add(RequestAttachmentRetention())
	return &at
}

// RequestAttachmentKind is what a sniffed content type is, for the decisions
// that depend on it: whether the file is held for a lead's release, and how a
// surface labels it. It is derived from the sniffed type, never from a name
// alone.
type RequestAttachmentKind string

const (
	// RequestAttachmentDocument is a file the team reads: a PDF, XML or HTML.
	RequestAttachmentDocument RequestAttachmentKind = "document"
	// RequestAttachmentImage is a raster picture.
	RequestAttachmentImage RequestAttachmentKind = "image"
	// RequestAttachmentText is plain text.
	RequestAttachmentText RequestAttachmentKind = "text"
	// RequestAttachmentArchive is a bundle of other files.
	RequestAttachmentArchive RequestAttachmentKind = "archive"
	// RequestAttachmentExecutable is code: a compiled binary, or whatever the
	// sniffer could not identify at all.
	RequestAttachmentExecutable RequestAttachmentKind = "executable"
)

// requestAttachmentTypeKinds is AMP's stored-type allowlist: the content types
// an attachment row may carry, each mapped to its kind. A type that is not
// here is refused.
//
// Every entry is a type Go's http.DetectContentType actually returns (see
// TestRequestAttachmentTypeAllowlistMatchesSniffedType). That is the point, not
// a coincidence: the stored type must be one the sniffer produces, so a client
// could not have chosen it. Types a sniffer never returns are therefore absent
// even when they would be convenient — application/json and text/csv arrive as
// text/plain, and image/svg+xml arrives as text/plain, so those files are
// stored as what was actually sniffed and refused nothing.
//
// The held kinds (archive, executable) are *accepted but held*: the approved
// mockup shows a customer attaching an .exe that waits for a lead, so the
// allowlist is about what may be stored, not what may download.
var requestAttachmentTypeKinds = map[string]RequestAttachmentKind{
	// Documents and text the team reads.
	"application/pdf": RequestAttachmentDocument,
	"text/html":       RequestAttachmentDocument,
	"text/xml":        RequestAttachmentDocument,
	"text/plain":      RequestAttachmentText,

	// Pictures.
	"image/png":  RequestAttachmentImage,
	"image/jpeg": RequestAttachmentImage,
	"image/gif":  RequestAttachmentImage,
	"image/webp": RequestAttachmentImage,
	"image/bmp":  RequestAttachmentImage,

	// Archives: held for a lead. Only the container formats the sniffer
	// recognizes by magic are named; a 7z, rar or bzip2 file sniffs as
	// application/octet-stream and is held as executable instead, which is the
	// stricter of the two.
	"application/zip":    RequestAttachmentArchive,
	"application/x-gzip": RequestAttachmentArchive,

	// Code, and the sniffer's fallback: http.DetectContentType returns
	// application/octet-stream for every binary it cannot name — a PE/EXE, an
	// ELF, a Mach-O, a 7z, an mp4 — so octet-stream *is* the state an unnamed
	// binary lands in. That is why it must be held rather than trusted or
	// refused: holding is recoverable ("ask for a different format", or a lead
	// releases it), and refusing would drop the file the customer thinks they
	// sent.
	"application/octet-stream": RequestAttachmentExecutable,
	"application/wasm":         RequestAttachmentExecutable,
}

// requestAttachmentScriptExtensions are name suffixes held as code even though
// the bytes sniff as text. Go's sniffer cannot tell a shell script from a
// shopping list (both are text/plain), so the approved mockup's "executables,
// scripts and archives of them are held" cannot be met from the sniffed type
// alone. These extensions use the requester's own name to hold *more* files,
// never fewer: a name can escalate a hold and can never lift one, so this
// stays compatible with "the stored type is the sniffed type, never the
// client's".
var requestAttachmentScriptExtensions = []string{
	".sh", ".bash", ".zsh", ".fish", ".ksh",
	".ps1", ".psm1", ".bat", ".cmd", ".com",
	".exe", ".dll", ".scr", ".msi",
	".jar", ".vbs", ".js", ".mjs", ".py", ".rb", ".pl", ".php",
}

// RequestAttachmentKindOf classifies a sniffed content type. ok is false for a
// type this file does not store; callers gating on the kind must treat that as
// unsafe rather than as ordinary.
func RequestAttachmentKindOf(contentType string) (RequestAttachmentKind, bool) {
	_, kind, ok := normalizeRequestAttachmentType(contentType)
	return kind, ok
}

// RequestAttachmentRestricted reports whether a kind is held for a team lead's
// release (contract §1's restricted kinds; artifact 37: "Executables, scripts
// and archives of them are held. A team lead can release after checking; the
// release is audited").
//
// Note what trusting the sniffed type costs: a .docx or .xlsx is a zip
// container, sniffing sees application/zip, and so an office file is held too.
// The alternative is believing the client's extension, which the contract
// forbids outright; the mockup's "ask for a different format" escape covers
// the common case.
func RequestAttachmentRestricted(kind RequestAttachmentKind) bool {
	return kind == RequestAttachmentArchive || kind == RequestAttachmentExecutable
}

// RequestAttachmentHeldByName reports whether the display name says code,
// whichever bytes arrived. See requestAttachmentScriptExtensions: this can hold
// a file the sniffed type did not, and can never release one it did.
func RequestAttachmentHeldByName(fileName string) bool {
	ext := strings.ToLower(path.Ext(strings.TrimSpace(fileName)))
	return slices.Contains(requestAttachmentScriptExtensions, ext)
}

// RequestAttachment is one private upload on a request: the metadata a team
// sees in the attachments panel, plus the retention, hold and scan columns the
// download, release and purge paths read.
type RequestAttachment struct {
	ID        int64 `json:"id"`
	RequestID int   `json:"request_id"`

	// FileName is the requester's name for the file, for display. It is never
	// a path, and it never decides the content type (only, at most, whether a
	// file is held).
	FileName string `json:"file_name"`
	// ContentType is the type AMP sniffed from the bytes, never the client's
	// Content-Type header (contract §1). The upload path sniffs and stores it;
	// Validate refuses a type a sniffer could not have produced.
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	// SHA256 is the bytes' digest, and the download's ETag (as for artifacts).
	SHA256 string `json:"sha256"`
	// ScanState gates download: see RequestScanState. Unscanned is not clean.
	ScanState RequestScanState `json:"scan_state"`

	UploadedBy *int      `json:"uploaded_by"`
	CreatedAt  time.Time `json:"created_at"`

	// D3 retention: on an active request nothing expires; 90 days after the
	// request closes the bytes are purged, unless keep pins them. ExpiresAt is
	// the stamped moment the purge job compares (see
	// RequestAttachmentExpiresAt); KeptAt/KeptBy record who pinned it.
	Keep      bool       `json:"keep"`
	KeptBy    *int       `json:"kept_by"`
	KeptAt    *time.Time `json:"kept_at"`
	ExpiresAt *time.Time `json:"expires_at"`

	// The restricted-file hold. ReleasedAt is when a team lead released a held
	// file after checking it by hand, and ReleasedBy who did. Only the
	// service's lead-only release path writes them; this file holds no
	// authorization decision, and the uploader or an agent can never be the
	// actor there.
	ReleasedAt *time.Time `json:"released_at"`
	ReleasedBy *int       `json:"released_by"`

	// Content is the stored bytes. The download path loads it; a list or an
	// event never carries it, and nothing in this file reads or writes it.
	Content []byte `json:"-"`
}

// Held reports whether the attachment waits for a team lead's release: a
// restricted kind, or a name that says code, that nobody has released. It
// fails closed — a content type this file cannot classify is held too.
func (a *RequestAttachment) Held() bool {
	if a.ReleasedAt != nil {
		return false
	}
	kind, ok := RequestAttachmentKindOf(a.ContentType)
	if !ok {
		return true
	}
	return RequestAttachmentRestricted(kind) || RequestAttachmentHeldByName(a.FileName)
}

// Expired reports whether the purge job (G4) may delete this attachment's
// bytes: a kept file is never purged, and a file with no expiry is on an
// active request (or already pinned).
func (a *RequestAttachment) Expired(now time.Time) bool {
	if a.Keep || a.ExpiresAt == nil {
		return false
	}
	return !now.Before(*a.ExpiresAt)
}

// RequestAttachmentRefusal says why an attachment's bytes may not be served.
// The zero value means the download may proceed.
type RequestAttachmentRefusal string

const (
	// RequestAttachmentServed: nothing refuses the download. It is still
	// served only through a short-lived signed URL and always as an
	// attachment (RequestAttachmentDisposition).
	RequestAttachmentServed RequestAttachmentRefusal = ""
	// RequestAttachmentRefusedInfected: a scanner found something.
	RequestAttachmentRefusedInfected RequestAttachmentRefusal = "scan_infected"
	// RequestAttachmentRefusedPending: no verdict has arrived yet.
	RequestAttachmentRefusedPending RequestAttachmentRefusal = "scan_pending"
	// RequestAttachmentRefusedHeld: a restricted kind or a code-like name that
	// no lead has released.
	RequestAttachmentRefusedHeld RequestAttachmentRefusal = "held_for_release"
	// RequestAttachmentRefusedPurged: the retention window passed.
	RequestAttachmentRefusedPurged RequestAttachmentRefusal = "purged"
)

// RequestAttachmentDownloadRefusal is the whole decision the download path
// makes about whether to serve bytes: "" means serve it, anything else is the
// refusal to report and the reason a surface shows. It is one pure function so
// the answer cannot differ between minting a signed URL and serving the bytes.
//
// Infected is tested before pending so a real verdict is the reason reported
// when both could apply, and the hold is tested before expiry because "a lead
// must release it" is the actionable answer.
func RequestAttachmentDownloadRefusal(a *RequestAttachment, now time.Time) RequestAttachmentRefusal {
	switch {
	case a.ScanState == RequestScanInfected:
		return RequestAttachmentRefusedInfected
	case a.ScanState.BlocksDownload():
		return RequestAttachmentRefusedPending
	case a.Held():
		return RequestAttachmentRefusedHeld
	case a.Expired(now):
		return RequestAttachmentRefusedPurged
	}
	return RequestAttachmentServed
}

// Validate normalizes the record's metadata in place and checks its shape: the
// request it belongs to, a display name that is not a path, a sniffed type AMP
// stores, a size within the per-file bound, a digest, a known scan state, and
// coherent keep/release columns. The collection bounds (count and total)
// belong to ValidateRequestAttachmentSet, because they are a property of the
// request, not of one row.
func (a *RequestAttachment) Validate() error {
	if a.RequestID <= 0 {
		return errors.New("an attachment belongs to exactly one request (request_id)")
	}
	name, err := NormalizeRequestAttachmentFileName(a.FileName)
	if err != nil {
		return err
	}
	contentType, kind, err := NormalizeRequestAttachmentContentType(a.ContentType)
	if err != nil {
		return err
	}
	if a.SizeBytes <= 0 {
		return errors.New("size_bytes must be positive: an empty file has nothing to attach")
	}
	if a.SizeBytes > MaxRequestAttachmentBytes {
		return fmt.Errorf("size_bytes is over the %d-byte per-file limit (got %d)", MaxRequestAttachmentBytes, a.SizeBytes)
	}
	sum, err := normalizeRequestAttachmentSHA256(a.SHA256)
	if err != nil {
		return err
	}
	scan, err := NormalizeRequestScanState(string(a.ScanState))
	if err != nil {
		return err
	}
	if a.UploadedBy != nil && *a.UploadedBy <= 0 {
		return errors.New("uploaded_by must be a user id")
	}
	// Keep and release record who and when as one-way implications, never a
	// biconditional: every user foreign key is ON DELETE SET NULL, so a stored
	// row keeps the moment after the person's row is gone (the same rule
	// team_requests uses in migration 053).
	if a.Keep && a.KeptAt == nil {
		return errors.New("a kept attachment records when it was pinned (kept_at)")
	}
	if a.KeptBy != nil {
		if *a.KeptBy <= 0 {
			return errors.New("kept_by must be a user id")
		}
		if a.KeptAt == nil {
			return errors.New("kept_by records when the keep happened (kept_at)")
		}
	}
	if a.ReleasedBy != nil {
		if *a.ReleasedBy <= 0 {
			return errors.New("released_by must be a user id")
		}
		if a.ReleasedAt == nil {
			return errors.New("released_by records when the release happened (released_at)")
		}
	}
	if a.ReleasedAt != nil && !RequestAttachmentRestricted(kind) && !RequestAttachmentHeldByName(name) {
		return errors.New("only a held attachment is released: this one was never restricted")
	}

	a.FileName, a.ContentType = name, contentType
	a.SHA256, a.ScanState = sum, scan
	return nil
}

// NormalizeRequestAttachmentFileName validates the display name: one line,
// trimmed, bounded, and never a path. A name carrying a separator or a ".."
// segment is refused here so no later layer has to think about zip-slip or
// about a filename breaking the Content-Disposition header.
func NormalizeRequestAttachmentFileName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", errors.New("file_name is required: it is what the requester and the team see")
	}
	if strings.ContainsAny(n, `/\`) || n == "." || n == ".." {
		return "", errors.New(`file_name is a display name, not a path: it must not contain "/" or "\"`)
	}
	for _, c := range n {
		if unicode.IsControl(c) {
			return "", errors.New("file_name must not contain control characters")
		}
	}
	if c := utf8.RuneCountInString(n); c > MaxRequestAttachmentNameLen {
		return "", fmt.Errorf("file_name is over %d characters (got %d)", MaxRequestAttachmentNameLen, c)
	}
	return n, nil
}

// NormalizeRequestAttachmentContentType validates a sniffed content type and
// returns it with its kind. Case is normalized and any parameter dropped:
// http.DetectContentType returns "text/plain; charset=utf-8", the bytes are
// always served as a download, so a charset buys nothing and a client-supplied
// one must never reach a header. A type a sniffer cannot produce is refused,
// which is how "the stored type is the sniffed type, never the client's" is
// enforced rather than merely documented.
func NormalizeRequestAttachmentContentType(contentType string) (string, RequestAttachmentKind, error) {
	t, kind, ok := normalizeRequestAttachmentType(contentType)
	if !ok {
		return "", "", fmt.Errorf("content_type %q is not a type AMP stores: PDF, XML, HTML, plain text and images are accepted, archives and executables are held for a lead, and anything else needs a different format", contentType)
	}
	return t, kind, nil
}

// normalizeRequestAttachmentType is the shared normalization behind the
// exported checks: trim, lower-case, drop parameters, then look up the
// allowlist.
func normalizeRequestAttachmentType(contentType string) (string, RequestAttachmentKind, bool) {
	t := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if t == "" {
		return "", "", false
	}
	kind, ok := requestAttachmentTypeKinds[t]
	return t, kind, ok
}

// normalizeRequestAttachmentSHA256 checks the digest is 64 hex characters and
// lower-cases it, so the same bytes always yield the same ETag.
func normalizeRequestAttachmentSHA256(s string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(s))
	if d == "" {
		return "", errors.New("sha256 is required: it is the bytes' digest and the download's ETag")
	}
	if len(d) != 64 {
		return "", fmt.Errorf("sha256 must be 64 hex characters (got %d)", len(d))
	}
	for _, c := range d {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", errors.New("sha256 must be 64 hex characters")
		}
	}
	return d, nil
}

// RequestAttachmentsTotalBytes sums a request's attachments: the number intake
// compares with MaxRequestAttachmentsTotalBytes.
func RequestAttachmentsTotalBytes(attachments []RequestAttachment) int64 {
	var total int64
	for _, a := range attachments {
		total += a.SizeBytes
	}
	return total
}

// ValidateRequestAttachmentSet applies the two collection bounds intake must
// check before writing anything: how many files one request carries and how
// many bytes they total. A request that already has attachments is checked
// against the same two numbers with the new set included, so the limit is on
// the request, not on one upload call.
func ValidateRequestAttachmentSet(attachments []RequestAttachment) error {
	if len(attachments) > MaxRequestAttachments {
		return fmt.Errorf("a request carries at most %d attachments (got %d): ask for the rest in a second request or a different format", MaxRequestAttachments, len(attachments))
	}
	if total := RequestAttachmentsTotalBytes(attachments); total > MaxRequestAttachmentsTotalBytes {
		return fmt.Errorf("a request's attachments total at most %d bytes (got %d)", MaxRequestAttachmentsTotalBytes, total)
	}
	return nil
}
