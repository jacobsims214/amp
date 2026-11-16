package service

// Tests for the authorized request-upload service (AMP #3961, epic 202 story
// 549; build slice G2's sniffing half of
// discoveries/team-inbox-approved-build-slices.md).
//
// The ticket's three criteria are what these assert, in order:
//
//  1. A spoofed MIME type is not stored: the row's content_type is the type Go's
//     sniffer produced from the bytes, whatever the file is called and whatever a
//     client would have declared, and a type AMP cannot represent is refused
//     (TestAddRequestAttachmentSniffsRatherThanTrusts). The count and total bounds
//     are enforced through this path too — a sixth file and a 61st MiB are both
//     refused with request.invalid and write nothing
//     (TestAddRequestAttachmentEnforcesCountAndTotalBounds).
//  2. Restricted content is HELD, not refused and not served: an archive, an
//     executable and a script-named text file are stored with the hold derived
//     from their sniffed type and name, the scan state is "unsupported" (never
//     clean), and the download decision refuses them
//     (TestAddRequestAttachmentHoldsRestrictedContent).
//  3. A foreign uploader is denied without a leak: a member of a sibling team in
//     the same org, a stranger from another org, another requester, and an id
//     that does not exist all get the surface's one not-found with identical
//     bytes, and nothing is written (TestAddRequestAttachmentRefusesForeignUploaders).
//
// Around the criteria the tests pin what else the path promises: who may upload
// (the requester to their own request, a team member to any request in the inbox)
// as a pure matrix (TestRequestUploadCapabilityFor), that a refused upload writes
// nothing and an accepted one is audited once without recording the file's name
// or bytes (TestAddRequestAttachmentAuditsTheStoredFacts), and that this file
// never grows a download path, an artifact dependency or a scanner claim
// (TestRequestUploadServiceStaysAnUploadPath).
//
// The database-backed tests reuse request_submit_test.go's fixture — a team
// linked to a project, a sibling team in the same org, another org with its own
// team, a member, an org owner, two customers and a stranger — and run against a
// real Postgres with every migration applied, when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/service -run 'AddRequestAttachment|RequestUploadCapability' -count=1 -race
//
// Every fixture row carries a run-unique suffix, so repeated runs and other tests
// in the same database never see each other's rows.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- fixtures ----

// uploadFixture is the submit fixture plus the two things these tests need that
// a submission does not: a request to attach to, and a second team in the same
// org with a member of its own (the "sibling team" that must not reach in).
type uploadFixture struct {
	*submitFixture
	// sibling is a member of otherTeam — same org, wrong team.
	sibling int
}

func newUploadFixture(t *testing.T) *uploadFixture {
	t.Helper()
	f := &uploadFixture{submitFixture: newSubmitFixture(t)}
	f.sibling = f.newUser("sibling", domain.UserKindHuman)
	f.exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, f.otherTeam, f.sibling)
	return f
}

// newUser inserts one more user row in this run's namespace.
func (f *uploadFixture) newUser(label, kind string) int {
	f.t.Helper()
	var id int
	if err := f.db.QueryRow(f.ctx,
		`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
		"upload-"+label+"-"+f.suffix(), "upload-"+label+"-"+f.suffix()+"@upload.test", kind).Scan(&id); err != nil {
		f.t.Fatalf("create user %s: %v", label, err)
	}
	return id
}

// suffix is a stable per-run token for rows this file creates beyond the submit
// fixture's own (which already carries one).
func (f *uploadFixture) suffix() string {
	f.t.Helper()
	var email string
	if err := f.db.QueryRow(f.ctx, `SELECT email FROM users WHERE id = $1`, f.member).Scan(&email); err != nil {
		f.t.Fatalf("read the fixture suffix: %v", err)
	}
	// submit-member-<n>@submit.test
	trimmed := strings.TrimPrefix(email, "submit-member-")
	return strings.TrimSuffix(trimmed, "@submit.test")
}

func (f *uploadFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// newRequest submits a request as userID and returns it: the record every upload
// in these tests attaches to, created through the real intake path so its tenant
// is the stored one rather than a hand-built row.
func (f *uploadFixture) newRequest(userID int, subject string) *domain.TeamRequest {
	f.t.Helper()
	res, err := f.svc.SubmitRequest(f.ctx, f.principal(userID), SubmitRequestInput{
		TeamID:     f.team,
		Source:     domain.RequestSourceWeb,
		Subject:    subject,
		InputBrief: "Please look at the attached file.",
	})
	if err != nil {
		f.t.Fatalf("submit request as %d: %v", userID, err)
	}
	return res.Request
}

// newForeignRequest submits a request to the OTHER org's team, so a request that
// exists but is in another tenant is part of the foreign-uploader matrix rather
// than only an id that does not exist.
func (f *uploadFixture) newForeignRequest() *domain.TeamRequest {
	f.t.Helper()
	res, err := f.svc.SubmitRequest(f.ctx, f.principal(f.stranger), SubmitRequestInput{
		TeamID:     f.foreign,
		Source:     domain.RequestSourceWeb,
		Subject:    "A request in another org",
		InputBrief: "Nobody in the first org may see this.",
	})
	if err != nil {
		f.t.Fatalf("submit a foreign request: %v", err)
	}
	return res.Request
}

// attachmentCount is how many files the request carries, read straight from the
// table so a refusal can be proved to have written nothing.
func (f *uploadFixture) attachmentCount(requestID int) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx,
		`SELECT count(*) FROM request_attachments WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		f.t.Fatalf("count attachments of %d: %v", requestID, err)
	}
	return n
}

// storedAttachment loads the row straight from the table: the tests assert on what
// Postgres holds, not only on what the method returned.
func (f *uploadFixture) storedAttachment(requestID int, fileName string) (contentType, scanState, sha256 string, size int, releasedAt *string, held bool) {
	f.t.Helper()
	var rel *string
	var releasedBy *int
	if err := f.db.QueryRow(f.ctx, `SELECT content_type, scan_state, sha256, size_bytes, released_at::text, released_by
		  FROM request_attachments WHERE request_id = $1 AND file_name = $2`, requestID, fileName).
		Scan(&contentType, &scanState, &sha256, &size, &rel, &releasedBy); err != nil {
		f.t.Fatalf("load stored attachment %q of %d: %v", fileName, requestID, err)
	}
	return contentType, scanState, sha256, size, rel, releasedBy != nil
}

// upload is one upload of content named fileName by userID.
func (f *uploadFixture) upload(userID int, req *domain.TeamRequest, fileName string, content []byte) (*domain.RequestAttachment, error) {
	f.t.Helper()
	return f.svc.AddRequestAttachment(f.ctx, f.principal(userID), AddRequestAttachmentInput{
		RequestID: req.ID,
		OrgID:     req.OrgID,
		TeamID:    req.TeamID,
		FileName:  fileName,
		Content:   content,
	})
}

// htmlBytes is an HTML document: what a client calls a screenshot and what the
// sniffer calls text/html.
func htmlBytes() []byte {
	return []byte("<!DOCTYPE html><html><body><script>fetch('/api/tokens')</script></body></html>\n")
}

// pngBytes is a minimal PNG header: the sniffer's image/png.
func pngBytes() []byte {
	return append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, make([]byte, 64)...)
}

// zipBytes is a PK archive header, which is also what an office file sniffs as.
func zipBytes() []byte {
	return append([]byte{'P', 'K', 0x03, 0x04}, make([]byte, 64)...)
}

// exeBytes is a PE header: the sniffer's application/octet-stream, and therefore
// the sniffer's answer for every binary it cannot name.
func exeBytes() []byte {
	return append([]byte{'M', 'Z', 0x90, 0x00}, make([]byte, 64)...)
}

// ---- criterion 1a: the stored type is the sniffed type ----

// TestAddRequestAttachmentSniffsRatherThanTrusts is criterion 1's MIME-spoof half:
// what the row stores is what http.DetectContentType said about the bytes, never
// what the name says and never what a client would have declared (the input type
// has no field for it at all), and a type AMP cannot represent is refused rather
// than stored as something else.
func TestAddRequestAttachmentSniffsRatherThanTrusts(t *testing.T) {
	f := newUploadFixture(t)

	cases := []struct {
		name        string
		fileName    string
		content     []byte
		wantType    string
		wantKind    domain.RequestAttachmentKind
		wantRefused bool
	}{
		// The headline spoof: HTML bytes under an image name. Stored as the HTML
		// the sniffer saw, held by nothing (HTML is a document) — and harmless
		// because nothing serves an attachment inline (G3's disposition).
		{"html named as a png", "screenshot.png", htmlBytes(), "text/html", domain.RequestAttachmentDocument, false},
		// The reverse: real PNG bytes under a text name. The name cannot lower the
		// stored type either.
		{"png named as text", "notes.txt", pngBytes(), "image/png", domain.RequestAttachmentImage, false},
		// Types a client would love to declare: JSON and CSV both sniff as
		// text/plain, which is the type actually stored.
		{"json", "payload.json", []byte(`{"admin": true}`), "text/plain", domain.RequestAttachmentText, false},
		{"csv", "rows.csv", []byte("a,b,c\n1,2,3\n"), "text/plain", domain.RequestAttachmentText, false},
		// SVG sniffs as text/xml — never image/svg+xml, which the sniffer does not
		// produce, so an SVG can never be stored as something a browser renders.
		{"svg", "logo.svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`),
			"text/xml", domain.RequestAttachmentDocument, false},
		// A type the sniffer produces but AMP does not store: refused, naming what
		// is accepted, rather than coerced into a type it is not.
		{"mp4", "clip.mp4", append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'}, make([]byte, 64)...),
			"", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A request per case: the count bound is the request's, so a shared
			// request would run out of files partway through the table.
			req := f.newRequest(f.customer, "MIME spoofing: "+tc.name)
			att, err := f.upload(f.customer, req, tc.fileName, tc.content)
			if tc.wantRefused {
				if err == nil {
					t.Fatalf("%q was accepted as %q; a type AMP does not store must be refused", tc.fileName, att.ContentType)
				}
				if code := authz.CodeOf(err); code != authz.CodeRequestInvalid {
					t.Fatalf("refusal code = %q (%v), want %q", code, err, authz.CodeRequestInvalid)
				}
				if n := f.attachmentCount(req.ID); n != 0 {
					t.Fatalf("a refused upload wrote %d rows", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("upload %q: %v", tc.fileName, err)
			}
			if att.ContentType != tc.wantType {
				t.Fatalf("returned content_type = %q, want %q (the sniffed type, never the name's)", att.ContentType, tc.wantType)
			}
			// The row is the proof: what the method returned and what Postgres holds
			// must be the same sniffed type.
			stored, _, _, size, rel, releasedBy := f.storedAttachment(req.ID, tc.fileName)
			if stored != tc.wantType {
				t.Fatalf("stored content_type = %q, want %q", stored, tc.wantType)
			}
			if size != len(tc.content) {
				t.Fatalf("stored size = %d, want %d (the bytes' length)", size, len(tc.content))
			}
			if rel != nil || releasedBy {
				t.Fatalf("an upload wrote a release (%v/%v): the release is a team lead's act, not the uploader's", rel, releasedBy)
			}
			// The bytes never come back from the upload path: it returns metadata, so
			// uploading can never be turned into a read.
			if att.Content != nil {
				t.Fatalf("the upload path returned %d bytes: it must return metadata only", len(att.Content))
			}
			if kind, ok := domain.RequestAttachmentKindOf(att.ContentType); !ok || kind != tc.wantKind {
				t.Fatalf("kind of %q = %q (ok=%v), want %q", att.ContentType, kind, ok, tc.wantKind)
			}
		})
	}

	// And the same file under two names is one type: the name is display only.
	same := f.newRequest(f.customer, "MIME spoofing: name is display only")
	if att, err := f.upload(f.customer, same, "one.txt", pngBytes()); err != nil || att.ContentType != "image/png" {
		t.Fatalf("png named one.txt: content_type = %v, err = %v; want image/png", att, err)
	}
}

// ---- criterion 1b: count and total bounds ----

// TestAddRequestAttachmentEnforcesCountAndTotalBounds is criterion 1's bounds
// half, through this path rather than the repository's: the per-file bound is
// answered before anything is written, and the request-level count and total
// bounds are the repository's numbers enforced under the request's row lock.
func TestAddRequestAttachmentEnforcesCountAndTotalBounds(t *testing.T) {
	f := newUploadFixture(t)
	req := f.newRequest(f.member, "bounds")

	// The per-file bound: 25 MiB + 1 byte is refused and writes nothing. It is
	// checked before the request is even loaded, so no row and no lock is touched.
	over := make([]byte, domain.MaxRequestAttachmentBytes+1)
	if _, err := f.upload(f.member, req, "huge.bin", over); err == nil {
		t.Fatal("a file over the 25 MiB per-file limit was accepted")
	} else if code := authz.CodeOf(err); code != authz.CodeRequestInvalid {
		t.Fatalf("over-per-file code = %q (%v), want %q", code, err, authz.CodeRequestInvalid)
	}
	// An empty file is refused too: it has nothing to attach.
	if _, err := f.upload(f.member, req, "empty.txt", nil); err == nil {
		t.Fatal("an empty file was accepted")
	} else if code := authz.CodeOf(err); code != authz.CodeRequestInvalid {
		t.Fatalf("empty-file code = %q (%v), want %q", code, err, authz.CodeRequestInvalid)
	}
	if n := f.attachmentCount(req.ID); n != 0 {
		t.Fatalf("refused uploads wrote %d rows", n)
	}

	// The count bound: five files fit, the sixth is request.invalid (a retry will
	// not fit, so 400 and not 409) and writes nothing.
	for i := 1; i <= domain.MaxRequestAttachments; i++ {
		if _, err := f.upload(f.member, req, "file"+strconv.Itoa(i)+".txt", []byte("file "+strconv.Itoa(i))); err != nil {
			t.Fatalf("upload %d of %d: %v", i, domain.MaxRequestAttachments, err)
		}
	}
	_, err := f.upload(f.member, req, "sixth.txt", []byte("one too many"))
	if err == nil {
		t.Fatal("a sixth attachment was accepted")
	}
	if code := authz.CodeOf(err); code != authz.CodeRequestInvalid {
		t.Fatalf("over-count code = %q (%v), want %q", code, err, authz.CodeRequestInvalid)
	}
	// The message names what was there under the lock, which is what the requester
	// can act on.
	if msg := err.Error(); !strings.Contains(msg, strconv.Itoa(domain.MaxRequestAttachments)) {
		t.Fatalf("over-count message %q does not name the %d-file bound", msg, domain.MaxRequestAttachments)
	}
	if n := f.attachmentCount(req.ID); n != domain.MaxRequestAttachments {
		t.Fatalf("%d attachments stored, want exactly %d", n, domain.MaxRequestAttachments)
	}

	// The TOTAL bound, on a fresh request: 3 × 16 MiB fits in 60 MiB, a fourth
	// would be 64 MiB. This is the case the count alone cannot catch — five files
	// under 25 MiB each can still total 125 MiB.
	big := f.newRequest(f.member, "total bound")
	chunk := make([]byte, 16<<20)
	for i := 1; i <= 3; i++ {
		if _, err := f.upload(f.member, big, "chunk"+strconv.Itoa(i)+".bin", chunk); err != nil {
			t.Fatalf("upload chunk %d: %v", i, err)
		}
	}
	_, err = f.upload(f.member, big, "chunk4.bin", chunk)
	if err == nil {
		t.Fatal("a fourth 16 MiB file was accepted: 64 MiB is over the 60 MiB total bound")
	}
	if code := authz.CodeOf(err); code != authz.CodeRequestInvalid {
		t.Fatalf("over-total code = %q (%v), want %q", code, err, authz.CodeRequestInvalid)
	}
	if n := f.attachmentCount(big.ID); n != 3 {
		t.Fatalf("%d attachments stored on the total-bound request, want exactly 3", n)
	}
	// A third request still gets its own budget: the bound is per request.
	other := f.newRequest(f.member, "another request")
	if _, err := f.upload(f.member, other, "chunk.bin", chunk); err != nil {
		t.Fatalf("a different request's first 16 MiB file: %v", err)
	}
}

// ---- criterion 2: restricted content is held, and nothing claims clean ----

// TestAddRequestAttachmentHoldsRestrictedContent is criterion 2: archives and
// executables (and a script-named text file) are STORED and HELD, not refused —
// the approved mockup shows a customer's .exe waiting for a lead — and the scan
// state is the honest "no verdict exists", never a clean claim.
func TestAddRequestAttachmentHoldsRestrictedContent(t *testing.T) {
	f := newUploadFixture(t)
	req := f.newRequest(f.customer, "restricted files")

	cases := []struct {
		name     string
		fileName string
		content  []byte
		wantType string
		held     bool
	}{
		// An archive, by magic bytes.
		{"zip", "bundle.zip", zipBytes(), "application/zip", true},
		// An executable: the sniffer's octet-stream fallback, which is every binary
		// it cannot name.
		{"exe", "berlin-rollout.exe", exeBytes(), "application/octet-stream", true},
		// A script: the bytes are plain text (the sniffer cannot tell a shell script
		// from a shopping list), so the NAME is what holds it. A name can escalate a
		// hold and can never lift one.
		{"script by name", "deploy.sh", []byte("#!/bin/sh\nrm -rf /\n"), "text/plain", true},
		// An office file: a docx is a zip container, so it is held as an archive. The
		// alternative is believing the client's extension, which the contract forbids.
		{"docx", "report.docx", zipBytes(), "application/zip", true},
		// An ordinary document is not held.
		{"pdf", "spec.pdf", append([]byte("%PDF-1.4\n"), make([]byte, 64)...), "application/pdf", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att, err := f.upload(f.customer, req, tc.fileName, tc.content)
			if err != nil {
				t.Fatalf("upload %q: %v (restricted files are stored and held, not refused)", tc.fileName, err)
			}
			if att.ContentType != tc.wantType {
				t.Fatalf("content_type = %q, want %q", att.ContentType, tc.wantType)
			}
			if got := att.Held(); got != tc.held {
				t.Fatalf("Held() = %v, want %v for %q", got, tc.held, tc.fileName)
			}
			// The scan state is explicit and honest. Unsupported is "no verdict
			// exists"; clean would be a claim AMP has no scanner to back, and pending
			// would hold every download behind a scanner that never runs.
			if att.ScanState != domain.RequestScanUnsupported {
				t.Fatalf("scan_state = %q, want %q (no scanner ran)", att.ScanState, domain.RequestScanUnsupported)
			}
			if att.ScanState.SaysClean() {
				t.Fatal("an upload is readable as scanned/clean: AMP ships no scanner")
			}
			stored, scan, _, _, rel, releasedBy := f.storedAttachment(req.ID, tc.fileName)
			if stored != tc.wantType || scan != string(domain.RequestScanUnsupported) {
				t.Fatalf("stored (%q, %q), want (%q, %q)", stored, scan, tc.wantType, domain.RequestScanUnsupported)
			}
			if rel != nil || releasedBy {
				t.Fatalf("an upload wrote a release for %q: a release is a team lead's audited act", tc.fileName)
			}
			// And the one shared download decision refuses the held file — this path
			// does not decide downloads, but the record it wrote must be refused by
			// the gate that does.
			if refusal := domain.RequestAttachmentDownloadRefusal(att, f.now); tc.held && refusal != domain.RequestAttachmentRefusedHeld {
				t.Fatalf("download refusal of a held %q = %q, want %q", tc.fileName, refusal, domain.RequestAttachmentRefusedHeld)
			}
			if refusal := domain.RequestAttachmentDownloadRefusal(att, f.now); !tc.held && refusal != domain.RequestAttachmentServed {
				t.Fatalf("download refusal of an unheld %q = %q, want served", tc.fileName, refusal)
			}
		})
	}
}

// ---- criterion 3: foreign uploaders are refused without a leak ----

// TestAddRequestAttachmentRefusesForeignUploaders is criterion 3: who may upload,
// and that every other caller gets the surface's ONE not-found — the same bytes an
// id that does not exist produces — with nothing written.
func TestAddRequestAttachmentRefusesForeignUploaders(t *testing.T) {
	f := newUploadFixture(t)
	// The request belongs to the customer, in the fixture's org and team.
	req := f.newRequest(f.customer, "who may attach")
	foreignReq := f.newForeignRequest()

	// The two positive cases first, so the refusals below are proved against a
	// request that demonstrably accepts uploads.
	if _, err := f.upload(f.customer, req, "mine.txt", []byte("the requester's own file")); err != nil {
		t.Fatalf("the requester uploading to their own request: %v", err)
	}
	if _, err := f.upload(f.member, req, "from-the-team.txt", []byte("a team member's file")); err != nil {
		t.Fatalf("a team member uploading to an inbox request: %v", err)
	}
	// An org owner is a team member through authz.InboxRole (owner/admin of the
	// team's org), so they may attach as well.
	if _, err := f.upload(f.admin, req, "from-an-owner.txt", []byte("an org owner's file")); err != nil {
		t.Fatalf("an org owner uploading to an inbox request: %v", err)
	}
	if n := f.attachmentCount(req.ID); n != 3 {
		t.Fatalf("%d attachments after three authorized uploads, want 3", n)
	}

	// Every refusal below must be the SAME answer as an id that does not exist.
	unknown, err := f.svc.AddRequestAttachment(f.ctx, f.principal(f.member), AddRequestAttachmentInput{
		RequestID: req.ID + 1_000_000, OrgID: req.OrgID, TeamID: req.TeamID,
		FileName: "probe.txt", Content: []byte("probe"),
	})
	if unknown != nil || err == nil {
		t.Fatalf("an unknown request id was accepted: %+v", unknown)
	}
	wantMsg := err.Error()
	wantStatus := authz.StatusOf(err, 500)
	if wantStatus != 404 || authz.CodeOf(err) != authz.CodeRequestNotFound {
		t.Fatalf("unknown request answered %d/%q (%v), want 404/%q", wantStatus, authz.CodeOf(err), err, authz.CodeRequestNotFound)
	}

	refusals := []struct {
		name   string
		user   int
		req    *domain.TeamRequest
		orgID  int
		teamID int
	}{
		// A member of a SIBLING team in the same org: the cross-team case the tenant
		// predicate exists for. Same org is not same team.
		{"sibling team member", f.sibling, req, req.OrgID, req.TeamID},
		// A person from another org entirely.
		{"stranger from another org", f.stranger, req, req.OrgID, req.TeamID},
		// Another requester: a customer with no grant and no membership.
		{"another requester", f.otherCust, req, req.OrgID, req.TeamID},
		// A request that exists in another org, addressed with its own real tenant:
		// the caller has no standing in that team.
		{"request in another org", f.sibling, foreignReq, foreignReq.OrgID, foreignReq.TeamID},
		// The right request addressed with the wrong tenant: an authorized caller
		// passing a team it does not belong to is a not-found, not a cross-tenant read.
		{"right request, wrong team", f.member, req, req.OrgID, f.otherTeam},
		{"right request, wrong org", f.member, req, f.otherOrg, req.TeamID},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			att, err := f.svc.AddRequestAttachment(f.ctx, f.principal(tc.user), AddRequestAttachmentInput{
				RequestID: tc.req.ID, OrgID: tc.orgID, TeamID: tc.teamID,
				FileName: "leak.txt", Content: []byte("should never be stored"),
			})
			if err == nil {
				t.Fatalf("%s uploaded an attachment: %+v", tc.name, att)
			}
			// Identical answer to an id that does not exist: status, code and message.
			if got := authz.StatusOf(err, 500); got != wantStatus {
				t.Fatalf("%s answered %d, want %d (%v)", tc.name, got, wantStatus, err)
			}
			if got := authz.CodeOf(err); got != authz.CodeRequestNotFound {
				t.Fatalf("%s answered code %q, want %q", tc.name, got, authz.CodeRequestNotFound)
			}
			if msg := err.Error(); msg != wantMsg {
				t.Fatalf("%s answered %q; an unknown id answers %q — the two must be identical or the id space leaks", tc.name, msg, wantMsg)
			}
			if n := f.attachmentCount(req.ID); n != 3 {
				t.Fatalf("%s wrote an attachment: %d rows, want 3", tc.name, n)
			}
			if n := f.attachmentCount(foreignReq.ID); n != 0 {
				t.Fatalf("%s wrote an attachment to the foreign request: %d rows", tc.name, n)
			}
		})
	}

	// A submit-only service credential reaches exactly one operation (its own
	// team's submission), so it may not upload to an existing request at all —
	// even on its bound team, and even when the account behind it is a member.
	scoped := f.principal(f.member)
	scoped.Scope, scoped.ScopeTeamID = domain.TokenScopeSubmit, f.team
	if _, err := f.svc.AddRequestAttachment(f.ctx, scoped, AddRequestAttachmentInput{
		RequestID: req.ID, OrgID: req.OrgID, TeamID: req.TeamID,
		FileName: "token.txt", Content: []byte("a scoped token's file"),
	}); err == nil {
		t.Fatal("a submit-only token uploaded an attachment")
	} else if code := authz.CodeOf(err); code != authz.CodeTokenScopeDenied {
		t.Fatalf("submit-only refusal code = %q (%v), want %q", code, err, authz.CodeTokenScopeDenied)
	}

	// A tenantless call is a caller bug, not a not-found: answering 404 would hide
	// it behind a legitimate refusal.
	if _, err := f.svc.AddRequestAttachment(f.ctx, f.principal(f.member), AddRequestAttachmentInput{
		RequestID: req.ID, FileName: "no-tenant.txt", Content: []byte("no tenant"),
	}); err == nil {
		t.Fatal("an upload with no tenant was accepted")
	}
}

// ---- the pure capability matrix ----

// TestRequestUploadCapabilityFor pins the whole of who may attach, without a
// database: the requester to their own request, a team member (including a lead,
// an org owner and a global admin, all of which authz.InboxRole reports as
// InboxMember) to any request in the inbox, and nobody else.
func TestRequestUploadCapabilityFor(t *testing.T) {
	requester := 41
	other := 42
	req := &domain.TeamRequest{ID: 7, RequesterUserID: &requester}
	serviceSubmitted := &domain.TeamRequest{ID: 8} // no requester user: a connector's

	cases := []struct {
		name string
		req  *domain.TeamRequest
		p    authz.Principal
		role authz.InboxRole
		want RequestUploadCapability
	}{
		{"the requester, no role", req, authz.Principal{UserID: requester, Channel: auth.ChannelUI}, authz.InboxNone, RequestUploadRequester},
		{"the requester through an API token", req, authz.Principal{UserID: requester, ViaToken: true, Channel: auth.ChannelToken}, authz.InboxNone, RequestUploadRequester},
		{"the requester is also a team member", req, authz.Principal{UserID: requester}, authz.InboxMember, RequestUploadTeam},
		{"a team member on someone else's request", req, authz.Principal{UserID: other}, authz.InboxMember, RequestUploadTeam},
		{"a stranger", req, authz.Principal{UserID: other}, authz.InboxNone, RequestUploadNone},
		{"the local-dev user with no id", req, authz.Principal{}, authz.InboxNone, RequestUploadNone},
		{"nobody can claim a service-submitted request", serviceSubmitted, authz.Principal{UserID: requester}, authz.InboxNone, RequestUploadNone},
		{"a team member on a service-submitted request", serviceSubmitted, authz.Principal{UserID: other}, authz.InboxMember, RequestUploadTeam},
		{"no request at all", nil, authz.Principal{UserID: requester}, authz.InboxMember, RequestUploadNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequestUploadCapabilityFor(tc.req, tc.p, tc.role); got != tc.want {
				t.Fatalf("capability = %s, want %s", got, tc.want)
			}
		})
	}
}

// ---- the audit row ----

// TestAddRequestAttachmentAuditsTheStoredFacts pins what the one audit row says:
// ids, the sniffed type, the size, the scan state and the derived hold — and not
// the file's name, its bytes or its digest, because none of those answers "who
// attached what to which request" and all of them are the requester's content.
func TestAddRequestAttachmentAuditsTheStoredFacts(t *testing.T) {
	f := newUploadFixture(t)
	req := f.newRequest(f.customer, "audited upload")

	att, err := f.upload(f.customer, req, "berlin-rollout.exe", exeBytes())
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	entries, err := f.svc.Repo.ListAudit(f.ctx, domain.AuditFilter{Action: "request.attachment.upload", TargetID: idStr(req.ID)})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d audit rows for one upload, want exactly 1", len(entries))
	}
	e := entries[0]
	if e.TargetType != "request" || e.TargetName != req.PublicRef {
		t.Fatalf("audit target = %q/%q, want request/%q", e.TargetType, e.TargetName, req.PublicRef)
	}
	if e.OrgID == nil || *e.OrgID != req.OrgID {
		t.Fatalf("audit org = %v, want %d", e.OrgID, req.OrgID)
	}
	if got := e.Details["content_type"]; got != att.ContentType {
		t.Fatalf("audit content_type = %v, want %q", got, att.ContentType)
	}
	if got := e.Details["scan_state"]; got != string(domain.RequestScanUnsupported) {
		t.Fatalf("audit scan_state = %v, want %q (the log must not read as evidence of a scan)", got, domain.RequestScanUnsupported)
	}
	if got := e.Details["held"]; got != true {
		t.Fatalf("audit held = %v, want true for an executable", got)
	}
	for _, banned := range []string{"file_name", "name", "content", "sha256"} {
		if _, ok := e.Details[banned]; ok {
			t.Fatalf("the audit row carries %q: the file's name and bytes are the requester's content, and the digest is not a fact the log needs", banned)
		}
	}
	if strings.Contains(e.Summary, "berlin-rollout") {
		t.Fatalf("the audit summary carries the file's name: %q", e.Summary)
	}

	// A refused upload writes no audit row: the log counts files, not attempts
	// (refusals are recorded once as access.denied by the serving listener).
	_, err = f.upload(f.sibling, req, "denied.txt", []byte("not mine"))
	if err == nil {
		t.Fatal("the sibling team's member uploaded")
	}
	after, err := f.svc.Repo.ListAudit(f.ctx, domain.AuditFilter{Action: "request.attachment.upload", TargetID: idStr(req.ID)})
	if err != nil {
		t.Fatalf("list audit after a refusal: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("%d audit rows after a refused upload, want still 1", len(after))
	}
}

// ---- the file stays an upload path ----

// TestRequestUploadServiceStaysAnUploadPath is the ticket's scope made checkable:
// this file sniffs, authorizes and stores, and nothing else. It parses the file
// (not its text, which is allowed to explain these rules) and fails if it grows
// a download decision, an artifact dependency, a public URL or a scan verdict.
func TestRequestUploadServiceStaysAnUploadPath(t *testing.T) {
	const file = "request_uploads.go"
	fs := token.NewFileSet()
	parsed, err := parser.ParseFile(fs, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	// The imports are the upload's whole vocabulary: the sniffer (net/http), the
	// digest, and the three internal layers this file sits between. No net/url, no
	// io, no artifact storage, no signing package.
	allowed := map[string]bool{
		"context":       true,
		"crypto/sha256": true,
		"encoding/hex":  true,
		"errors":        true,
		"fmt":           true,
		"net/http":      true,
		"github.com/simstech/amp-api/internal/authz":      true,
		"github.com/simstech/amp-api/internal/domain":     true,
		"github.com/simstech/amp-api/internal/repository": true,
	}
	imported := map[string]bool{}
	for _, spec := range parsed.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("import path %s: %v", spec.Path.Value, err)
		}
		imported[path] = true
		if !allowed[path] {
			t.Errorf("%s imports %q: the upload path has no business fetching, serving or publishing anything", file, path)
		}
	}
	if !imported["net/http"] {
		t.Errorf("%s does not import net/http: sniffing is this file's whole job, and nowhere else may decide the stored type", file)
	}

	// Identifiers this file must never name, and why.
	forbidden := []struct{ name, why string }{
		{"RequestScanClean", "clean is a claim only a scanner may make, and AMP ships none"},
		{"RequestAttachmentDownloadRefusal", "whether bytes may be READ is the download gate's decision (#3963), not the upload's"},
		{"RequestAttachmentDisposition", "the serving posture belongs to the download gate"},
		{"ContentDisposition", "the upload path sets no headers"},
		{"PublishArtifact", "an attachment is private and separate from an artifact (contract §1)"},
		{"artifactContentType", "the artifact sniffer serves a different primitive with different rules"},
		{"ArtifactHTML", "an attachment is never stored as an artifact"},
	}
	ast.Inspect(parsed, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.Ident:
			for _, f := range forbidden {
				if node.Name == f.name {
					t.Errorf("%s uses %q: %s", file, node.Name, f.why)
				}
			}
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(node.Value)
			if err != nil {
				return true
			}
			// No server-side fetch and no public URL: the bytes arrive in memory or
			// they do not arrive (056's header makes the same point for the table).
			for _, banned := range []string{"http://", "https://", "Content-Disposition", "url", "URL"} {
				if strings.Contains(value, banned) {
					t.Errorf("%s carries the literal %q: no fetch, no public address and no header belongs on the upload path", file, value)
					break
				}
			}
		}
		return true
	})

	// The scan state this path writes is the honest one, and the sniffer really is
	// the one Go ships — asserted positively so a later edit cannot swap in a
	// client-supplied type without failing here.
	if requestUploadScanState != domain.RequestScanUnsupported {
		t.Fatalf("requestUploadScanState = %q: an upload must land on the no-verdict state", requestUploadScanState)
	}
	if got := http.DetectContentType(htmlBytes()); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("the sniffer answered %q for HTML: the stored-type vocabulary this file relies on changed", got)
	}
}
