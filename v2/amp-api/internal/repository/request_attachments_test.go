package repository

// Tests for request_attachments.go against a real Postgres (AMP #3955, epic 202
// story 548; build slice G2's storage half). They run only when TEST_DATABASE_URL is
// set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository/ -run 'RequestAttachment' -count=1 -race
//
// The ticket's three criteria are what these assert, in order:
//
//  1. metadata and bytes round-trip under a tenant: an insert returns the stored
//     record with the size equal to the bytes and the digest computed from them, the
//     list and the single read return the metadata with Content nil, the byte read
//     returns exactly the bytes that went in, and every one of those reads is scoped
//     by org AND team — a request, attachment or byte read in another team of the same
//     org, or in another org, is the same ErrNotFound as a missing id.
//  2. the count and total bounds hold under concurrency: N uploads at once to one
//     request leave exactly the number the bounds allow (five files, or as many as fit
//     in 60 MiB), every loser is *RequestAttachmentBounds naming what the request
//     already held, no loser writes anything, and a second request gets its own five
//     (the bound is the request's, not the table's).
//  3. no public or foreign-team read, and no false clean state: nothing in the storage
//     layer names a serving path or a scanner verdict (a test parses the file's
//     imports and text), an unset scan state is stored as 'unsupported' and never as a
//     clean claim, and a restricted file's derived hold plus its release are preserved
//     exactly as written.
//
// Beyond the criteria the tests pin the properties the code's comments claim: the size
// is the bytes' length (there is no declared-size field to lie with), a caller-supplied
// digest that disagrees with the bytes is refused rather than stored, a client-style
// content type a sniffer cannot produce is refused, keep/expires columns survive a
// round-trip unchanged, a release on a file that was never held is refused by the
// domain's rule (not merely by the schema), the column list matches the live table, and
// a delete removes the bytes with the row and is idempotent.
//
// Each test builds its own orgs, teams, users and requests with a random suffix
// (requestRowsFixture), so repeated runs and tests in the same database don't see each
// other's rows.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// requestAttachmentsFixture is requestRowsFixture plus a team in the OTHER org (the
// tenant a cross-org read must never reach) and the helpers these tests share.
type requestAttachmentsFixture struct {
	*requestRowsFixture
	teamOtherOrg int
}

func newRequestAttachmentsFixture(t *testing.T) *requestAttachmentsFixture {
	t.Helper()
	f := &requestAttachmentsFixture{requestRowsFixture: newRequestRowsFixture(t)}
	if err := f.r.db.QueryRow(f.ctx,
		`INSERT INTO teams (org_id, name) VALUES ($1, 'Elsewhere desk') RETURNING id`,
		f.other).Scan(&f.teamOtherOrg); err != nil {
		t.Fatalf("insert team in the other org: %v", err)
	}
	return f
}

// newRequest writes one `new` request in the fixture's first team and returns it as
// stored (id, rev=1, no claim, no owner).
func (f *requestAttachmentsFixture) newRequest(label string) *domain.TeamRequest {
	f.t.Helper()
	base := requestTime(-time.Hour)
	req := &domain.TeamRequest{
		PublicRef:        f.ref("att" + label),
		OrgID:            f.org,
		TeamID:           f.team,
		RequesterContact: "requester@attachments.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "Attach to me " + label,
		State:            domain.RequestStatusNew,
		Rev:              1,
		Metadata:         domain.Metadata{},
		CreatedAt:        base,
		UpdatedAt:        base,
	}
	req.ID = f.insertRequest(req)
	return req
}

// otherTeamRequest writes one request in the fixture's SECOND team — the same org, a
// different tenant — which every attachment read must treat as not-found.
func (f *requestAttachmentsFixture) otherTeamRequest(label string) *domain.TeamRequest {
	f.t.Helper()
	req := f.newRequest(label)
	if _, err := f.r.db.Exec(f.ctx, `UPDATE team_requests SET team_id = $2 WHERE id = $1`, req.ID, f.team2); err != nil {
		f.t.Fatalf("move request %d to the second team: %v", req.ID, err)
	}
	req.TeamID = f.team2
	return req
}

// otherOrgRequest writes one request in the other org's own team.
func (f *requestAttachmentsFixture) otherOrgRequest(label string) *domain.TeamRequest {
	f.t.Helper()
	base := requestTime(-time.Hour)
	req := &domain.TeamRequest{
		PublicRef:        f.ref("attother" + label),
		OrgID:            f.other,
		TeamID:           f.teamOtherOrg,
		RequesterContact: "requester@attachments.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "Elsewhere " + label,
		State:            domain.RequestStatusNew,
		Rev:              1,
		Metadata:         domain.Metadata{},
		CreatedAt:        base,
		UpdatedAt:        base,
	}
	req.ID = f.insertRequest(req)
	return req
}

// write is the caller's part of an upload into this fixture's first request, with the
// fields the tests vary left to the caller.
func (f *requestAttachmentsFixture) write(req *domain.TeamRequest, name, contentType string, content []byte) RequestAttachmentInsert {
	return RequestAttachmentInsert{
		RequestID:   req.ID,
		OrgID:       req.OrgID,
		TeamID:      req.TeamID,
		FileName:    name,
		ContentType: contentType,
		Content:     content,
		UploadedBy:  &f.user,
	}
}

// mustInsert uploads one file and fails the test if it is refused.
func (f *requestAttachmentsFixture) mustInsert(req *domain.TeamRequest, name, contentType string, content []byte) *domain.RequestAttachment {
	f.t.Helper()
	a, err := f.r.InsertRequestAttachment(f.ctx, f.write(req, name, contentType, content))
	if err != nil {
		f.t.Fatalf("insert attachment %q on request %d: %v", name, req.ID, err)
	}
	return a
}

// attRef builds the caller's part of an attachment read for this fixture's request.
func (f *requestAttachmentsFixture) attRef(req *domain.TeamRequest, a *domain.RequestAttachment) RequestAttachmentRef {
	return RequestAttachmentRef{
		AttachmentID: a.ID,
		RequestID:    req.ID,
		OrgID:        req.OrgID,
		TeamID:       req.TeamID,
	}
}

// countAttachments counts the request's stored rows straight from the table, so an
// assertion sees what was actually written.
func (f *requestAttachmentsFixture) countAttachments(requestID int) int {
	f.t.Helper()
	var n int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_attachments WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		f.t.Fatalf("count attachments of request %d: %v", requestID, err)
	}
	return n
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestRequestAttachmentRoundTrip is criterion 1's first half: metadata and bytes go in
// and come back out under the request's tenant, with the size equal to the bytes and
// the digest computed from them — and the list/metadata read does not carry the bytes
// at all.
func TestRequestAttachmentRoundTrip(t *testing.T) {
	f := newRequestAttachmentsFixture(t)
	req := f.newRequest("roundtrip")

	content := []byte("%PDF-1.4\n%âãÏÓ\n1 0 obj\n<< /Type /Catalog >>\nendobj\n")
	stored := f.mustInsert(req, "invoice.pdf", "application/pdf", content)

	if stored.ID <= 0 {
		t.Fatalf("insert returned attachment id %d", stored.ID)
	}
	if stored.SizeBytes != int64(len(content)) {
		t.Errorf("size_bytes = %d, want the bytes' length %d", stored.SizeBytes, len(content))
	}
	if want := digestOf(content); stored.SHA256 != want {
		t.Errorf("sha256 = %s, want the computed %s", stored.SHA256, want)
	}
	if stored.ScanState != domain.RequestScanUnsupported {
		t.Errorf("scan_state = %q, want the unsupported default (no scanner ran)", stored.ScanState)
	}
	if stored.UploadedBy == nil || *stored.UploadedBy != f.user {
		t.Errorf("uploaded_by = %v, want %d", stored.UploadedBy, f.user)
	}
	if len(stored.Content) != 0 {
		t.Errorf("insert returned %d bytes; the stored record is metadata only", len(stored.Content))
	}

	// The list: metadata, in upload order, no bytes.
	list, err := f.r.ListRequestAttachments(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d attachments, want 1", len(list))
	}
	if list[0].Content != nil {
		t.Error("the list carries content: bytes must never travel with the metadata")
	}
	if list[0].FileName != "invoice.pdf" || list[0].ContentType != "application/pdf" ||
		list[0].SHA256 != digestOf(content) || list[0].SizeBytes != int64(len(content)) {
		t.Errorf("listed attachment = %+v, want the stored metadata", list[0])
	}

	// The single metadata read agrees with the list.
	one, err := f.r.GetRequestAttachment(f.ctx, f.attRef(req, &list[0]))
	if err != nil {
		t.Fatalf("get attachment: %v", err)
	}
	if one.Content != nil {
		t.Error("GetRequestAttachment returned content: only LoadRequestAttachment reads bytes")
	}

	// The byte read: the exact bytes, with the same metadata.
	loaded, err := f.r.LoadRequestAttachment(f.ctx, f.attRef(req, &list[0]))
	if err != nil {
		t.Fatalf("load attachment: %v", err)
	}
	if !bytes.Equal(loaded.Content, content) {
		t.Errorf("loaded %d bytes, want the %d that were stored", len(loaded.Content), len(content))
	}
	if loaded.SHA256 != digestOf(loaded.Content) {
		t.Errorf("stored sha256 %s is not the digest of the stored bytes: the ETag would lie", loaded.SHA256)
	}
	if loaded.FileName != "invoice.pdf" || loaded.ContentType != "application/pdf" {
		t.Errorf("loaded record = %+v, want the stored file name and sniffed type", loaded)
	}

	// Two more files, inserted in order: the list is by id, not by name or time.
	f.mustInsert(req, "b-notes.txt", "text/plain", []byte("first\n"))
	f.mustInsert(req, "a-notes.txt", "text/plain", []byte("second\n"))
	list, err = f.r.ListRequestAttachments(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	names := []string{list[0].FileName, list[1].FileName, list[2].FileName}
	wantNames := []string{"invoice.pdf", "b-notes.txt", "a-notes.txt"}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Errorf("upload order = %v, want %v", names, wantNames)
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].ID >= list[i].ID {
			t.Errorf("ids %d then %d: the list must be in ascending id order", list[i-1].ID, list[i].ID)
		}
	}
}

// TestRequestAttachmentBoundsAreTheRequestConcurrent pins criterion 2 for the count:
// eight uploads at once to one request leave exactly five rows, and every loser is a
// *RequestAttachmentBounds that wrote nothing. The limit is the request's, so a second
// request still gets its own five.
//
// This is deterministic rather than scheduler luck: every upload locks the request row
// first, so the second writer to take the lock counts the first one's committed row,
// and the sixth finds five.
func TestRequestAttachmentBoundsAreTheRequestConcurrent(t *testing.T) {
	f := newRequestAttachmentsFixture(t)
	req := f.newRequest("racecount")

	const n = 8 // more than MaxRequestAttachments (5)
	start := make(chan struct{})
	type result struct {
		i   int
		a   *domain.RequestAttachment
		err error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			content := []byte(fmt.Sprintf("file %d\n", i))
			<-start
			a, err := f.r.InsertRequestAttachment(f.ctx,
				f.write(req, fmt.Sprintf("file-%d.txt", i), "text/plain", content))
			results <- result{i: i, a: a, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var ok, refused int
	for res := range results {
		switch {
		case res.err == nil:
			ok++
			if res.a == nil {
				t.Errorf("upload %d succeeded with a nil record", res.i)
			}
		case errors.Is(res.err, ErrRequestAttachmentBounds):
			refused++
			var over *RequestAttachmentBounds
			if !errors.As(res.err, &over) {
				t.Errorf("upload %d: err = %v, want *RequestAttachmentBounds naming what was stored", res.i, res.err)
				continue
			}
			if over.RequestID != req.ID {
				t.Errorf("upload %d: refusal names request %d, want %d", res.i, over.RequestID, req.ID)
			}
			if over.Existing < domain.MaxRequestAttachments-1 || over.Existing > domain.MaxRequestAttachments {
				t.Errorf("upload %d: refusal says %d files existed, want %d or %d",
					res.i, over.Existing, domain.MaxRequestAttachments-1, domain.MaxRequestAttachments)
			}
			// The domain's message names the bound; the wrapper must keep it.
			if over.Err == nil {
				t.Errorf("upload %d: refusal carries no cause, so it cannot name the bound", res.i)
			} else if !strings.Contains(over.Err.Error(), strconv.Itoa(domain.MaxRequestAttachments)) {
				t.Errorf("upload %d: refusal cause %q does not name the %d-file bound", res.i, over.Err, domain.MaxRequestAttachments)
			}
		default:
			t.Fatalf("upload %d: err = %v, want success or an over-count refusal", res.i, res.err)
		}
	}
	if ok != domain.MaxRequestAttachments {
		t.Errorf("%d uploads succeeded, want exactly %d", ok, domain.MaxRequestAttachments)
	}
	if refused != n-domain.MaxRequestAttachments {
		t.Errorf("%d uploads were refused, want %d", refused, n-domain.MaxRequestAttachments)
	}
	if got := f.countAttachments(req.ID); got != domain.MaxRequestAttachments {
		t.Fatalf("request holds %d attachments, want %d", got, domain.MaxRequestAttachments)
	}

	// A loser wrote nothing: the five stored rows all carry their own bytes' digest and
	// size, so a half-written sixth row would show up as a sixth id.
	loaded, err := f.r.ListRequestAttachments(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	for _, a := range loaded {
		body, err := f.r.LoadRequestAttachment(f.ctx, f.attRef(req, &a))
		if err != nil {
			t.Fatalf("load attachment %d: %v", a.ID, err)
		}
		if int64(len(body.Content)) != a.SizeBytes || digestOf(body.Content) != a.SHA256 {
			t.Errorf("attachment %d (%s) does not match its stored size/digest", a.ID, a.FileName)
		}
	}

	// The bound is the request's: another request in the same team gets its own five.
	second := f.newRequest("racecount2")
	for i := 0; i < domain.MaxRequestAttachments; i++ {
		f.mustInsert(second, fmt.Sprintf("s-%d.txt", i), "text/plain", []byte("ok\n"))
	}
	if _, err := f.r.InsertRequestAttachment(f.ctx,
		f.write(second, "one-too-many.txt", "text/plain", []byte("no\n"))); !errors.Is(err, ErrRequestAttachmentBounds) {
		t.Fatalf("a sixth file on the second request: err = %v, want an over-count refusal", err)
	}
}

// TestRequestAttachmentTotalBytesBoundConcurrent pins criterion 2 for the total: four
// 16 MiB uploads at once on one request leave three (48 MiB), because a fourth would be
// 64 MiB — over MaxRequestAttachmentsTotalBytes — while four files are still well under
// the count bound. That is the case the count alone cannot catch, and it is why both
// checks exist.
func TestRequestAttachmentTotalBytesBoundConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("the total-bytes race allocates 4 x 16 MiB")
	}
	f := newRequestAttachmentsFixture(t)
	req := f.newRequest("racetotal")

	// 16 MiB per file: under the 25 MiB per-file bound, and 3 x 16 = 48 MiB fits the
	// 60 MiB total while 4 x 16 = 64 MiB does not.
	perFile := 16 << 20
	if perFile*4 <= domain.MaxRequestAttachmentsTotalBytes {
		t.Fatalf("the fixture's file size proves nothing: 4 x %d does not exceed %d",
			perFile, domain.MaxRequestAttachmentsTotalBytes)
	}

	const n = 4
	start := make(chan struct{})
	results := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Incompressible-ish content: an index byte per block, so the payload is not
			// a zero page (056's no-orphan-bytes test makes the same point about TOAST).
			content := make([]byte, perFile)
			for j := range content {
				content[j] = byte(j%251) ^ byte(i)
			}
			<-start
			_, err := f.r.InsertRequestAttachment(f.ctx,
				f.write(req, fmt.Sprintf("bulk-%d.bin", i), "application/octet-stream", content))
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var ok int
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrRequestAttachmentBounds):
			var over *RequestAttachmentBounds
			if !errors.As(err, &over) {
				t.Errorf("refusal = %v, want *RequestAttachmentBounds", err)
				continue
			}
			if over.TotalBytes < int64(perFile)*1 {
				t.Errorf("refusal says only %d bytes were stored, which cannot be over the total", over.TotalBytes)
			}
			if over.TotalBytes+int64(perFile) <= domain.MaxRequestAttachmentsTotalBytes {
				t.Errorf("refusal says %d bytes were stored and this file would make %d, which is under the %d total",
					over.TotalBytes, over.TotalBytes+int64(perFile), domain.MaxRequestAttachmentsTotalBytes)
			}
		default:
			t.Fatalf("upload err = %v, want success or an over-total refusal", err)
		}
	}
	if ok != 3 {
		t.Fatalf("%d uploads succeeded, want 3 (3 x 16 MiB = 48 MiB fits, 4 = 64 MiB does not)", ok)
	}
	var total int64
	if err := f.r.db.QueryRow(f.ctx,
		`SELECT COALESCE(SUM(size_bytes),0) FROM request_attachments WHERE request_id = $1`, req.ID).Scan(&total); err != nil {
		t.Fatalf("sum sizes of request %d: %v", req.ID, err)
	}
	if total > domain.MaxRequestAttachmentsTotalBytes {
		t.Fatalf("request holds %d bytes, over the %d total: a race wrote past the bound", total, domain.MaxRequestAttachmentsTotalBytes)
	}
	if got := f.countAttachments(req.ID); got != 3 {
		t.Fatalf("request holds %d attachments, want 3", got)
	}
}

// TestRequestAttachmentTenantScope is criterion 3's second half: no foreign-team read,
// in any of the four shapes (list, metadata, bytes, delete), and no foreign write. A
// request, attachment or request id in another team of the same org, or in another org,
// is ErrNotFound — the same answer a missing id gets — so the read cannot even tell an
// id it may not see from one that does not exist.
func TestRequestAttachmentTenantScope(t *testing.T) {
	f := newRequestAttachmentsFixture(t)
	mine := f.newRequest("mine")
	content := []byte("the team's private file\n")
	att := f.mustInsert(mine, "secret.txt", "text/plain", content)

	otherTeam := f.otherTeamRequest("otherteam")
	otherOrg := f.otherOrgRequest("otherorg")

	// Same-org, other team: the ids are real, the tenant is not this caller's.
	for _, tc := range []struct {
		name   string
		req    *domain.TeamRequest
		org    int
		team   int
		detail string
	}{
		{"other team, same org", otherTeam, otherTeam.OrgID, otherTeam.TeamID, "a sibling team of the same org"},
		{"other org", otherOrg, otherOrg.OrgID, otherOrg.TeamID, "another org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The attachment id is real and belongs to MY request; the tenant named is the
			// other one. That is the shape a handler bug produces: the id came from a body
			// or a path, the tenant from the session.
			ref := RequestAttachmentRef{AttachmentID: att.ID, RequestID: mine.ID, OrgID: tc.org, TeamID: tc.team}
			if _, err := f.r.LoadRequestAttachment(f.ctx, ref); !errors.Is(err, ErrNotFound) {
				t.Errorf("LoadRequestAttachment with %s's tenant: err = %v, want ErrNotFound", tc.detail, err)
			}
			if _, err := f.r.GetRequestAttachment(f.ctx, ref); !errors.Is(err, ErrNotFound) {
				t.Errorf("GetRequestAttachment with %s's tenant: err = %v, want ErrNotFound", tc.detail, err)
			}
			changed, err := f.r.DeleteRequestAttachment(f.ctx, ref)
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("DeleteRequestAttachment with %s's tenant: err = %v, want ErrNotFound", tc.detail, err)
			}
			if changed {
				t.Errorf("DeleteRequestAttachment with %s's tenant reported a delete", tc.detail)
			}
			if _, err := f.r.ListRequestAttachments(f.ctx, mine.ID, tc.org, tc.team); !errors.Is(err, ErrNotFound) {
				t.Errorf("ListRequestAttachments with %s's tenant: err = %v, want ErrNotFound", tc.detail, err)
			}
			// And a write into the other request's tenant with this caller's org/team pair
			// must not land.
			if _, err := f.r.InsertRequestAttachment(f.ctx, RequestAttachmentInsert{
				RequestID: tc.req.ID, OrgID: f.org, TeamID: f.team,
				FileName: "injected.txt", ContentType: "text/plain", Content: []byte("no\n"),
			}); !errors.Is(err, ErrNotFound) {
				t.Errorf("insert into %s's request from this tenant: err = %v, want ErrNotFound", tc.detail, err)
			}
			if got := f.countAttachments(tc.req.ID); got != 0 {
				t.Errorf("%s's request holds %d attachments after the refused write, want 0", tc.detail, got)
			}
		})
	}

	// The attachment is still there and still readable by its own tenant: every refusal
	// above wrote nothing.
	loaded, err := f.r.LoadRequestAttachment(f.ctx, f.attRef(mine, att))
	if err != nil {
		t.Fatalf("the owner's read was affected: %v", err)
	}
	if !bytes.Equal(loaded.Content, content) {
		t.Errorf("the owner's bytes changed")
	}

	// An attachment id that does not exist, and one that belongs to another request in
	// this same tenant, are the same not-found as a foreign tenant.
	other := f.newRequest("mine2")
	otherAtt := f.mustInsert(other, "other.txt", "text/plain", []byte("other\n"))
	if _, err := f.r.LoadRequestAttachment(f.ctx, RequestAttachmentRef{
		AttachmentID: otherAtt.ID, RequestID: mine.ID, OrgID: f.org, TeamID: f.team,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("another request's attachment id: err = %v, want ErrNotFound", err)
	}
	if _, err := f.r.LoadRequestAttachment(f.ctx, RequestAttachmentRef{
		AttachmentID: att.ID + 1_000_000, RequestID: mine.ID, OrgID: f.org, TeamID: f.team,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown attachment id: err = %v, want ErrNotFound", err)
	}

	// A caller with no tenant at all is a scope bug, not a 404: it must not be able to
	// reach a row by id alone.
	if _, err := f.r.LoadRequestAttachment(f.ctx, RequestAttachmentRef{AttachmentID: att.ID, RequestID: mine.ID}); !errors.Is(err, ErrRequestTenantScope) {
		t.Errorf("a tenantless read: err = %v, want ErrRequestTenantScope", err)
	}
}

// TestRequestAttachmentPreservesHoldScanAndRetention pins criterion 3's third half and
// the preservation rule: an unset scan state is stored as 'unsupported' — never as a
// clean claim — a restricted type's derived hold is computed from the stored record,
// a lead's release round-trips, and keep/kept_by/kept_at/expires_at survive unchanged.
func TestRequestAttachmentPreservesHoldScanAndRetention(t *testing.T) {
	f := newRequestAttachmentsFixture(t)
	req := f.newRequest("hold")

	// A zip sniffs as application/zip, which is a restricted kind: held until released.
	zipBytes := []byte("PK\x03\x04 a zip the sniffer recognizes\n")
	zip := f.mustInsert(req, "bundle.zip", "application/zip", zipBytes)
	if zip.ScanState != domain.RequestScanUnsupported {
		t.Errorf("scan_state = %q, want unsupported", zip.ScanState)
	}
	if zip.ScanState.SaysClean() {
		t.Fatal("an unscanned file is being reported as clean: that is a security claim AMP cannot back")
	}
	if !zip.Held() {
		t.Error("an application/zip attachment is not held: the sniffed type decides the hold")
	}
	if refusal := domain.RequestAttachmentDownloadRefusal(zip, time.Now()); refusal != domain.RequestAttachmentRefusedHeld {
		t.Errorf("download refusal = %q, want %q (held for release)", refusal, domain.RequestAttachmentRefusedHeld)
	}

	// A code-like NAME holds a file whose bytes sniff as text: a name can escalate a
	// hold and can never lift one.
	script := f.mustInsert(req, "deploy.sh", "text/plain", []byte("#!/bin/sh\necho hi\n"))
	if !script.Held() {
		t.Error("deploy.sh (text/plain) is not held: a code-like name must escalate the hold")
	}

	// A plain text file is not held, and releasing it is refused by the domain — the
	// schema's CHECK is the same rule, so a hand-written row cannot release it either.
	plain := f.mustInsert(req, "notes.txt", "text/plain", []byte("shopping list\n"))
	if plain.Held() {
		t.Error("notes.txt is held, want an ordinary document")
	}
	now := requestTime(-time.Minute)
	if _, err := f.r.InsertRequestAttachment(f.ctx, RequestAttachmentInsert{
		RequestID: req.ID, OrgID: req.OrgID, TeamID: req.TeamID,
		FileName: "notes2.txt", ContentType: "text/plain", Content: []byte("more notes\n"),
		ReleasedAt: &now, ReleasedBy: &f.user,
	}); err == nil {
		t.Fatal("a release on a file that was never held was accepted")
	}

	// A release on a held file is recorded and lifts the hold. The file's scan state is
	// untouched: release is not a scan verdict.
	released := f.mustInsert(req, "tool.zip", "application/zip", zipBytes)
	if _, err := f.r.db.Exec(f.ctx, `UPDATE request_attachments SET released_at = $2, released_by = $3 WHERE id = $1`,
		released.ID, now, f.user); err != nil {
		t.Fatalf("release attachment %d: %v", released.ID, err)
	}
	reloaded, err := f.r.GetRequestAttachment(f.ctx, f.attRef(req, released))
	if err != nil {
		t.Fatalf("reload released attachment: %v", err)
	}
	if reloaded.ReleasedAt == nil || !reloaded.ReleasedAt.Equal(now) {
		t.Errorf("released_at = %v, want %s", reloaded.ReleasedAt, now.UTC())
	}
	if reloaded.ReleasedBy == nil || *reloaded.ReleasedBy != f.user {
		t.Errorf("released_by = %v, want %d", reloaded.ReleasedBy, f.user)
	}
	if reloaded.Held() {
		t.Error("a released attachment is still held")
	}
	if reloaded.ScanState.SaysClean() {
		t.Error("a release turned into a scan verdict: releasing a file is not scanning it")
	}

	// D3 retention columns survive a round-trip exactly as written.
	expires := requestTime(90 * 24 * time.Hour)
	keptAt := requestTime(-2 * time.Hour)
	kept := f.mustInsert(req, "kept.txt", "text/plain", []byte("pinned\n"))
	if _, err := f.r.db.Exec(f.ctx, `UPDATE request_attachments
		SET keep = TRUE, kept_by = $2, kept_at = $3, expires_at = $4 WHERE id = $1`,
		kept.ID, f.user, keptAt, expires); err != nil {
		t.Fatalf("keep attachment %d: %v", kept.ID, err)
	}
	reloaded, err = f.r.GetRequestAttachment(f.ctx, f.attRef(req, kept))
	if err != nil {
		t.Fatalf("reload kept attachment: %v", err)
	}
	if !reloaded.Keep {
		t.Error("keep did not survive the round-trip")
	}
	if reloaded.KeptAt == nil || !reloaded.KeptAt.Equal(keptAt) {
		t.Errorf("kept_at = %v, want %s", reloaded.KeptAt, keptAt.UTC())
	}
	if reloaded.KeptBy == nil || *reloaded.KeptBy != f.user {
		t.Errorf("kept_by = %v, want %d", reloaded.KeptBy, f.user)
	}
	if reloaded.ExpiresAt == nil || !reloaded.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %v, want %s", reloaded.ExpiresAt, expires.UTC())
	}
	if reloaded.Expired(requestTime(0)) {
		t.Error("a kept file reports itself purgable: keep must pin it")
	}
}

// TestRequestAttachmentSizeIsTheBytesAndDigestIsChecked pins the two claims that make a
// stored row trustworthy: the size is the length that arrived (there is no field to
// declare it), and a caller-supplied digest is verified against the bytes rather than
// stored on trust.
func TestRequestAttachmentSizeIsTheBytesAndDigestIsChecked(t *testing.T) {
	f := newRequestAttachmentsFixture(t)
	req := f.newRequest("size")

	// The digest the caller computed over different bytes: refused, nothing written.
	content := []byte("the real bytes\n")
	in := f.write(req, "mismatch.txt", "text/plain", content)
	in.SHA256 = digestOf([]byte("some other bytes entirely\n"))
	if _, err := f.r.InsertRequestAttachment(f.ctx, in); !errors.Is(err, ErrRequestAttachmentDigest) {
		t.Fatalf("a mismatched digest: err = %v, want ErrRequestAttachmentDigest", err)
	}
	if got := f.countAttachments(req.ID); got != 0 {
		t.Fatalf("%d attachments were written by the refused insert, want 0", got)
	}

	// The caller's digest in a non-normalized form is accepted and stored normalized: an
	// upper-case hex digest of the same bytes is the same digest.
	in = f.write(req, "upper.txt", "text/plain", content)
	in.SHA256 = strings.ToUpper(digestOf(content))
	a, err := f.r.InsertRequestAttachment(f.ctx, in)
	if err != nil {
		t.Fatalf("an upper-case digest of the right bytes: %v", err)
	}
	if a.SHA256 != digestOf(content) {
		t.Errorf("stored sha256 = %q, want the lower-case %q", a.SHA256, digestOf(content))
	}

	// Empty content is refused by the domain (an empty file has nothing to attach), and
	// an over-per-file-limit payload is refused, so 056's size CHECKs are never the
	// first thing that catches a bad upload.
	if _, err := f.r.InsertRequestAttachment(f.ctx, f.write(req, "empty.txt", "text/plain", nil)); err == nil {
		t.Error("empty content was accepted")
	}
	over := make([]byte, domain.MaxRequestAttachmentBytes+1)
	if _, err := f.r.InsertRequestAttachment(f.ctx, f.write(req, "huge.bin", "application/octet-stream", over)); err == nil {
		t.Error("a file over the per-file limit was accepted")
	}

	// A type a sniffer cannot produce is refused: this is the storage half of "the stored
	// type is the sniffed type, never the client's header".
	for _, clientType := range []string{"application/json", "text/csv", "image/svg+xml", "video/mp4", "application/x-msdownload"} {
		if _, err := f.r.InsertRequestAttachment(f.ctx,
			f.write(req, "client-type.dat", clientType, []byte("{}\n"))); err == nil {
			t.Errorf("content_type %q was accepted", clientType)
		}
	}
	// A parameterized type is the one case that is accepted and CHANGED rather than
	// refused: the domain drops the parameter so the column holds the bare sniffed type
	// (http.DetectContentType really does return "text/plain; charset=utf-8", and 056's
	// CHECK refuses the parameterized form). A client-supplied charset therefore never
	// reaches a row, and the stored value is identical to the unparameterized upload's.
	normalized, err := f.r.InsertRequestAttachment(f.ctx,
		f.write(req, "charset.txt", "text/plain; charset=utf-8", []byte("body\n")))
	if err != nil {
		t.Fatalf("a parameterized text/plain type: %v", err)
	}
	if normalized.ContentType != "text/plain" {
		t.Errorf("stored content_type = %q, want the parameter dropped (\"text/plain\")", normalized.ContentType)
	}
	if changed, err := f.r.DeleteRequestAttachment(f.ctx, f.attRef(req, normalized)); err != nil || !changed {
		t.Fatalf("clean up the normalized upload: changed=%v err=%v", changed, err)
	}
	// A path in the name is refused, and so is a name that is not one.
	for _, badName := range []string{"../escape.txt", "a/b.txt", `a\b.txt`, "", ".."} {
		if _, err := f.r.InsertRequestAttachment(f.ctx,
			f.write(req, badName, "text/plain", []byte("x\n"))); err == nil {
			t.Errorf("file_name %q was accepted", badName)
		}
	}
	if got := f.countAttachments(req.ID); got != 1 {
		t.Fatalf("request holds %d attachments, want only the valid one", got)
	}
}

// TestDeleteRequestAttachmentRemovesTheBytesAndIsIdempotent: a delete takes the row and
// therefore, because the bytes live in the row, the bytes; a second delete of the same
// id answers changed=false rather than an error (a retry after a lost response must be
// safe), and the request's other files are untouched.
func TestDeleteRequestAttachmentRemovesTheBytesAndIsIdempotent(t *testing.T) {
	f := newRequestAttachmentsFixture(t)
	req := f.newRequest("delete")

	keep := f.mustInsert(req, "keep.txt", "text/plain", []byte("keep me\n"))
	drop := f.mustInsert(req, "drop.txt", "text/plain", []byte("drop me\n"))

	changed, err := f.r.DeleteRequestAttachment(f.ctx, f.attRef(req, drop))
	if err != nil {
		t.Fatalf("delete attachment: %v", err)
	}
	if !changed {
		t.Fatal("the first delete reported no change")
	}
	if _, err := f.r.LoadRequestAttachment(f.ctx, f.attRef(req, drop)); !errors.Is(err, ErrNotFound) {
		t.Errorf("the deleted attachment's bytes are still readable: err = %v, want ErrNotFound", err)
	}
	again, err := f.r.DeleteRequestAttachment(f.ctx, f.attRef(req, drop))
	if err != nil {
		t.Fatalf("the second delete: %v", err)
	}
	if again {
		t.Error("the second delete reported a change; a delete must be idempotent")
	}
	list, err := f.r.ListRequestAttachments(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	if len(list) != 1 || list[0].ID != keep.ID {
		t.Fatalf("the request lists %+v, want only attachment %d", list, keep.ID)
	}
}

// TestRequestAttachmentColumnsMatchSchema is the guard behind the two column lists: the
// full list must be exactly the table's columns, and the metadata list must be exactly
// the full list minus `content`. A migration that adds a column and forgets this file
// fails here; so does a metadata read that quietly starts carrying the bytes.
func TestRequestAttachmentColumnsMatchSchema(t *testing.T) {
	f := newRequestAttachmentsFixture(t)
	split := func(list string) map[string]bool {
		t.Helper()
		set := map[string]bool{}
		for _, c := range strings.Split(strings.ReplaceAll(list, "\n", " "), ",") {
			name := strings.TrimSpace(c)
			if name == "" {
				t.Fatalf("column list has an empty entry: %q", list)
			}
			if set[name] {
				t.Errorf("column list lists %q twice", name)
			}
			set[name] = true
		}
		return set
	}
	full := split(requestAttachmentCols(""))
	meta := split(requestAttachmentMetaCols(""))

	rows, err := f.r.db.Query(f.ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'request_attachments'`)
	if err != nil {
		t.Fatalf("columns of request_attachments: %v", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columns of request_attachments: %v", err)
	}
	rows.Close()

	for name := range have {
		if !full[name] {
			t.Errorf("request_attachments.%s is not in requestAttachmentCols: the scanner would read the wrong column", name)
		}
		if name != "content" && !meta[name] {
			t.Errorf("request_attachments.%s is not in the metadata list: a metadata read would drop it", name)
		}
	}
	for name := range full {
		if !have[name] {
			t.Errorf("requestAttachmentCols names %q, which request_attachments does not have", name)
		}
	}
	if len(full) != len(have) {
		t.Errorf("requestAttachmentCols covers %d columns but request_attachments has %d", len(full), len(have))
	}
	// The bytes are in exactly one list, and it is not the metadata one.
	if !full["content"] {
		t.Error("requestAttachmentCols does not list content: the bytes read would return nothing")
	}
	if meta["content"] {
		t.Error("the metadata list carries content: a list read would load up to 25 MiB per row")
	}
	if len(meta) != len(full)-1 {
		t.Errorf("the metadata list has %d columns, want %d (the full list minus content)", len(meta), len(full)-1)
	}

	// The alias form qualifies every column, which is what makes the join unambiguous:
	// both request_attachments and team_requests have `id` and `created_at`.
	aliased := requestAttachmentMetaCols("a")
	for _, col := range []string{"a.id", "a.request_id", "a.created_at", "a.content_type"} {
		if !strings.Contains(aliased, col) {
			t.Errorf("the aliased list is missing %q: %s", col, aliased)
		}
	}
	if strings.Contains(aliased, ", id") || strings.HasPrefix(aliased, "id,") {
		t.Errorf("the aliased list has an unqualified column (ambiguous in a join): %s", aliased)
	}
}

// TestRequestAttachmentStorageLayerStaysMetadataAndBytes is criterion 3's first half as
// a test rather than a promise: the storage layer imports no network, no sniffing and no
// serving package, and never names a clean scan verdict. A storage path that can write
// "clean" is one that can claim a scan that never ran; a storage path that can build a
// URL is one that can serve bytes outside the download gate.
//
// It parses the file instead of searching its text, so the check is about WHAT THE CODE
// USES rather than what its comments happen to say: the comment above the scan-state
// field is allowed to explain that this layer never writes "clean" — that sentence is the
// design, and a plain text search flags it (which is what the first version of this test
// did, on its own prose).
func TestRequestAttachmentStorageLayerStaysMetadataAndBytes(t *testing.T) {
	const file = "request_attachments.go"
	fs := token.NewFileSet()
	// ImportsOnly | ParseComments would give comments too; neither flag group is used, so
	// doc and line comments are not part of the tree this walks.
	parsed, err := parser.ParseFile(fs, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	// No package that could sniff, serve or fetch. crypto/sha256 and encoding/hex are the
	// digest and nothing else; domain is the record, the bounds and the hold rules; pgx is
	// the one driver this package talks to.
	allowed := map[string]bool{
		"context":                 true,
		"crypto/sha256":           true,
		"encoding/hex":            true,
		"errors":                  true,
		"fmt":                     true,
		"strings":                 true,
		"time":                    true,
		"github.com/jackc/pgx/v5": true,
		"github.com/simstech/amp-api/internal/domain": true,
	}
	for _, spec := range parsed.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("import path %s: %v", spec.Path.Value, err)
		}
		if !allowed[path] {
			t.Errorf("%s imports %q: the storage layer must stay free of network, sniffing and serving code", file, path)
		}
	}

	// No identifier that would sniff a type, claim a scan verdict, or name the serving
	// posture. RequestScanClean is the domain's constant for "a scanner ran and found
	// nothing": nothing in this package may reach for it, so an unsupported file cannot be
	// stored as clean by a storage-layer mistake.
	forbidden := map[string]string{
		"RequestScanClean":             "a storage layer that can write a clean verdict can claim a scan that never ran",
		"DetectContentType":            "sniffing belongs to the upload service, which sniffs once and passes the type",
		"ContentDisposition":           "serving posture belongs to the download gate, not the store",
		"RequestAttachmentDisposition": "serving posture belongs to the download gate, not the store",
	}
	ast.Inspect(parsed, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.Ident:
			if why, bad := forbidden[node.Name]; bad {
				t.Errorf("%s uses %q: %s", file, node.Name, why)
			}
		case *ast.BasicLit:
			// A URL or a header spelled as a string is the other way a serving path appears.
			if node.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(node.Value)
			if err != nil {
				return true
			}
			for _, banned := range []string{"http://", "https://", "Content-Disposition"} {
				if strings.Contains(value, banned) {
					t.Errorf("%s carries the literal %q: no public or header-building path belongs in the store", file, value)
					break
				}
			}
		}
		return true
	})
}

// TestRequestAttachmentEmptyRequestList: a request with no files lists an empty slice,
// not nil and not an error, so a panel renders "no attachments" rather than a JSON null.
func TestRequestAttachmentEmptyRequestList(t *testing.T) {
	f := newRequestAttachmentsFixture(t)
	req := f.newRequest("empty")

	list, err := f.r.ListRequestAttachments(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	if list == nil {
		t.Fatal("an empty request's attachment list is nil: it must be an empty slice")
	}
	if len(list) != 0 {
		t.Fatalf("listed %d attachments on a fresh request", len(list))
	}
	// An unknown request is a 404, not an empty list: "no files" and "no such request" must
	// not render the same.
	if _, err := f.r.ListRequestAttachments(f.ctx, req.ID+1_000_000, req.OrgID, req.TeamID); !errors.Is(err, ErrNotFound) {
		t.Errorf("list of an unknown request: err = %v, want ErrNotFound", err)
	}
}

// TestRequestAttachmentInsertDoesNotWriteHistory pins the boundary this slice keeps: an
// attachment is not a lifecycle transition, so the insert writes exactly one row and
// appends no request_events row. The service that records the act owns its history.
func TestRequestAttachmentInsertDoesNotWriteHistory(t *testing.T) {
	f := newRequestAttachmentsFixture(t)
	req := f.newRequest("history")
	f.mustInsert(req, "silent.txt", "text/plain", []byte("no event\n"))

	var events int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_events WHERE request_id = $1`, req.ID).Scan(&events); err != nil {
		t.Fatalf("count events of request %d: %v", req.ID, err)
	}
	if events != 0 {
		t.Errorf("%d history rows were written by an upload, want 0: an attachment is not a transition", events)
	}
	// And the request's own row did not move: no rev bump, no state change. An upload is
	// not an edit of the request's content.
	stored, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		t.Fatalf("GetRequest(%d): %v", req.ID, err)
	}
	if stored.Rev != req.Rev {
		t.Errorf("rev moved from %d to %d on an upload", req.Rev, stored.Rev)
	}
	if stored.State != req.State {
		t.Errorf("state moved from %q to %q on an upload", req.State, stored.State)
	}
}

// TestRequestAttachmentForeignUploaderIsNotFound: a departed uploader (a user id with no
// users row) is an ordinary not-found rather than a constraint error escaping as a 5xx,
// matching the claim and link paths.
func TestRequestAttachmentForeignUploaderIsNotFound(t *testing.T) {
	f := newRequestAttachmentsFixture(t)
	req := f.newRequest("ghost")
	ghost := 200_000_000 + f.suffix
	keptAt := requestTime(-time.Minute)
	in := f.write(req, "ghost.txt", "text/plain", []byte("who sent this?\n"))
	in.UploadedBy = &ghost
	if _, err := f.r.InsertRequestAttachment(f.ctx, in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an upload by a nonexistent user: err = %v, want ErrNotFound", err)
	}
	if got := f.countAttachments(req.ID); got != 0 {
		t.Fatalf("%d attachments were written by the refused insert", got)
	}

	// A keep or release attributed to a user who does not exist is the same not-found, and
	// the message names which column failed: the three user references are different bugs
	// to a caller, so one message for all three would hide which one is wrong.
	if _, err := f.r.InsertRequestAttachment(f.ctx, RequestAttachmentInsert{
		RequestID: req.ID, OrgID: req.OrgID, TeamID: req.TeamID,
		FileName: "ghost-keep.txt", ContentType: "text/plain", Content: []byte("kept by nobody\n"),
		Keep: true, KeptBy: &ghost, KeptAt: &keptAt,
	}); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "kept_by") {
		t.Errorf("a keep by a nonexistent user: err = %v, want ErrNotFound naming kept_by", err)
	}
	if _, err := f.r.InsertRequestAttachment(f.ctx, RequestAttachmentInsert{
		RequestID: req.ID, OrgID: req.OrgID, TeamID: f.team, FileName: "ghost-release.zip",
		ContentType: "application/zip", Content: []byte("PK\x03\x04 zip\n"),
		ReleasedAt: &keptAt, ReleasedBy: &ghost,
	}); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "released_by") {
		t.Errorf("a release by a nonexistent user: err = %v, want ErrNotFound naming released_by", err)
	}

	// Uploading without an AMP user is allowed: a connector or service client may have
	// none, and the file is still the request's.
	in = f.write(req, "service.txt", "text/plain", []byte("from a connector\n"))
	in.UploadedBy = nil
	a, err := f.r.InsertRequestAttachment(f.ctx, in)
	if err != nil {
		t.Fatalf("an upload with no uploader: %v", err)
	}
	if a.UploadedBy != nil {
		t.Errorf("uploaded_by = %v, want nil", a.UploadedBy)
	}
}
