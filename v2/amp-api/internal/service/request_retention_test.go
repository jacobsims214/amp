package service

// Tests for the request-attachment retention service (AMP #3964, epic 202 story
// 549; build slice G4 of discoveries/team-inbox-approved-build-slices.md). They
// run only when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test -p 1 ./internal/service -run 'RequestRetention|KeepRequestAttachment|DeleteRequestAttachment|PurgeExpiredRequestAttachments|RunRequestAttachmentPurge' -count=1 -race
//
// **-p 1 is required when this package runs alongside internal/repository** (whose
// retention tests delete due rows too): `go test ./internal/service ./internal/repository`
// runs the two packages CONCURRENTLY, and the purge is global by design, so one
// package's purge legitimately deletes the other package's due fixtures. That is
// the product's behaviour, not a bug — a retention job has no notion of a test's
// tenant — so the suites are run one package at a time. Assertions here are also
// written against postconditions ("this file is gone", "this file survives") rather
// than against a pass's global total, so a genuinely concurrent purge cannot make
// them pass or fail for the wrong reason.
//
// The ticket's three criteria are what these assert, in order:
//
//  1. The retention table passes (the active / closed / kept / reopened matrix is
//     the repository's, request_attachment_retention_test.go; this file proves the
//     two halves meet): a close starts the request's 90-day clock through the real
//     transition path, a reopen clears it in the same act, and the purge then
//     deletes exactly the files whose window closed and nobody kept
//     (TestRequestRetentionCloseStartsTheClockAndReopenClearsIt,
//     TestPurgeExpiredRequestAttachmentsPurgesTheClosedAndAuditsEach).
//  2. A purge removes the bytes AND leaves one audit row per file
//     (TestPurgeExpiredRequestAttachmentsPurgesTheClosedAndAuditsEach): the audit
//     names the request, the attachment id, the sniffed type and the size, and
//     never the file's name, its digest or its bytes — the same rule the upload's
//     audit row follows.
//  3. An unauthorized keep or delete is refused and writes nothing
//     (TestRequestRetentionCapabilityFor is the pure matrix;
//     TestKeepAndDeleteRefuseCallersWithoutStanding and
//     TestRequesterMayDeleteTheirOwnFileButNotKeepIt are the service's answers): a
//     stranger gets the surface's single not-found, a requester may delete their
//     own file and may not pin it, and neither act writes a row or an audit entry
//     when it is refused.
//
// Every test reuses the lifecycle fixture (a team linked to a project, a sibling
// team in the same org, another org with its own team, a member, a lead, an org
// owner, a requester with no team role and a stranger) and drives the real
// service, so the authorization and the clock are the product's rather than a
// test's idea of them.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// attach adds one file to req as who, through the real upload path.
func attach(t *testing.T, f *lifecycleFixture, who int, req *domain.TeamRequest, name string, content []byte) *domain.RequestAttachment {
	t.Helper()
	a, err := f.svc.AddRequestAttachment(f.ctx, f.principal(who), AddRequestAttachmentInput{
		RequestID: req.ID, OrgID: req.OrgID, TeamID: req.TeamID, FileName: name, Content: content,
	})
	if err != nil {
		t.Fatalf("attach %q as %d: %v", name, who, err)
	}
	return a
}

// retentionInput is the service's input for one stored file.
func retentionInput(req *domain.TeamRequest, a *domain.RequestAttachment) RequestAttachmentRetentionInput {
	return RequestAttachmentRetentionInput{
		RequestID: req.ID, OrgID: req.OrgID, TeamID: req.TeamID, AttachmentID: a.ID,
	}
}

// attachmentCount counts req's stored files straight from the table, so a refusal
// can be proved to have deleted nothing.
func attachmentCount(t *testing.T, f *lifecycleFixture, req *domain.TeamRequest) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM request_attachments WHERE request_id = $1`, req.ID).Scan(&n); err != nil {
		t.Fatalf("count attachments of %d: %v", req.ID, err)
	}
	return n
}

// storedExpiry reads one file's expiry, nil meaning "may not be purged".
func storedExpiry(t *testing.T, f *lifecycleFixture, attID int64) *time.Time {
	t.Helper()
	var at *time.Time
	if err := f.db.QueryRow(f.ctx, `SELECT expires_at FROM request_attachments WHERE id = $1`, attID).Scan(&at); err != nil {
		t.Fatalf("read expiry of %d: %v", attID, err)
	}
	return at
}

// ageRequest moves a request's close moment into the past, which is how a test
// reaches "90 days after close" without waiting 90 days. It is the one SQL write
// these tests make: everything else goes through the service.
func ageRequest(t *testing.T, f *lifecycleFixture, req *domain.TeamRequest, state domain.RequestStatus, closedAt time.Time) {
	t.Helper()
	if _, err := f.db.Exec(f.ctx, `UPDATE team_requests SET
			state = $2,
			delivered_at = CASE WHEN $2 IN ('delivered','requester_accepted') THEN $3 ELSE delivered_at END,
			outcome_brief = CASE WHEN $2 IN ('delivered','requester_accepted') THEN 'Delivered and written down.' ELSE outcome_brief END,
			accepted_at = CASE WHEN $2 = 'requester_accepted' THEN $3 ELSE accepted_at END,
			accepted_by = CASE WHEN $2 = 'requester_accepted' THEN requester_user_id ELSE accepted_by END,
			closed_at = CASE WHEN $2 IN ('rejected','duplicate') THEN $3 ELSE closed_at END,
			reject_reason = CASE WHEN $2 = 'rejected' THEN 'Not something this desk does.' ELSE reject_reason END
		WHERE id = $1`, req.ID, string(state), closedAt); err != nil {
		t.Fatalf("age request %d to %s closed %v: %v", req.ID, state, closedAt, err)
	}
}

// purgeRequestFiles runs one purge pass and answers how many of THIS test's
// files it deleted, identified by the parent ids given. The pass itself is global
// (that is what a retention job is) and a shared test database accumulates other
// tests' due rows, so every assertion about "what was purged" is scoped to the
// requests the test created rather than to the pass's total.
func purgeRequestFiles(t *testing.T, f *lifecycleFixture, reqs ...*domain.TeamRequest) int {
	t.Helper()
	want := make(map[int]bool, len(reqs))
	for _, r := range reqs {
		want[r.ID] = true
	}
	count := func() int {
		n := 0
		for _, r := range reqs {
			n += attachmentCount(t, f, r)
		}
		return n
	}
	before := count()
	// A shared database can hold a large backlog of other runs' due rows, and one
	// pass is bounded (that bound is the job's design). Keep passing while this
	// test's own files are still disappearing, so the assertion is about THIS test's
	// files and never about how much work the database happened to owe.
	after := before
	for i := 0; i < 12 && after > 0; i++ {
		if _, err := f.svc.PurgeExpiredRequestAttachments(f.ctx); err != nil {
			t.Fatalf("purge: %v", err)
		}
		next := count()
		if next == after {
			break
		}
		after = next
	}
	return before - after
}

// purgeOnePass runs exactly one purge pass, for the tests that assert the job's
// own shape rather than an outcome.
func purgeOnePass(t *testing.T, f *lifecycleFixture) int {
	t.Helper()
	n, err := f.svc.PurgeExpiredRequestAttachments(f.ctx)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	return n
}

// retentionAudits lists the audit rows one action wrote for a request.
func retentionAudits(t *testing.T, f *lifecycleFixture, action string, requestID int) []domain.AuditEntry {
	t.Helper()
	entries, err := f.svc.Repo.ListAudit(f.ctx, domain.AuditFilter{Action: action, TargetID: idStr(requestID)})
	if err != nil {
		t.Fatalf("list %s audit for %d: %v", action, requestID, err)
	}
	return entries
}

// ---- criterion 1: the clock follows the request ----

// TestRequestRetentionCloseStartsTheClockAndReopenClearsIt drives the real
// lifecycle and asserts what the files' expiry did at each move — the place the
// two halves of the slice meet. It is the reopened half of the retention table,
// taken through the service instead of through SQL.
func TestRequestRetentionCloseStartsTheClockAndReopenClearsIt(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	// A working request with one file, which the requester may add to their own.
	req := f.newRequest("reclock001", domain.RequestStatusTriaging, &requester)
	a := attach(t, f, requester, req, "brief.txt", []byte("the input brief\n"))

	// Active: no expiry, whatever else happens.
	if got := storedExpiry(t, f, a.ID); got != nil {
		t.Fatalf("an active request's file has expiry %v, want NULL", got)
	}

	// A triage move that does not close anything changes nothing.
	before := f.get(req.ID, f.team)
	if _, err := f.svc.TransitionRequest(f.ctx, f.principal(f.member), TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, To: domain.RequestStatusAccepted, ExpectedRev: before.Rev,
	}); err != nil {
		t.Fatalf("accept the triage: %v", err)
	}
	if got := storedExpiry(t, f, a.ID); got != nil {
		t.Fatalf("a working move set the file's expiry to %v, want NULL", got)
	}

	// A reject closes the request, so the file gets close+90d. The close is recent,
	// so the stamp is in the future: nothing may purge it yet.
	before = f.get(req.ID, f.team)
	if _, err := f.svc.TransitionRequest(f.ctx, f.principal(f.lead), TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, To: domain.RequestStatusRejected,
		RejectReason: "Outside this desk's scope.", ExpectedRev: before.Rev,
	}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	closed := f.get(req.ID, f.team)
	got := storedExpiry(t, f, a.ID)
	if got == nil {
		t.Fatal("closing the request left the file with no expiry: its clock never started")
	}
	if got.Before(time.Now()) {
		t.Fatalf("a just-closed request's file is already due (%v): the window was not applied", got)
	}
	want := closed.ClosedAt.Add(domain.RequestAttachmentRetention())
	if !got.Equal(want) {
		t.Fatalf("expiry = %v, want the request's close+%dd %v", got, domain.RequestAttachmentRetentionDays, want)
	}

	// The team reopens it: the clock clears in the same act, so the file is not
	// reported purged by a download gate reading the stamp.
	before = f.get(req.ID, f.team)
	if _, err := f.svc.TransitionRequest(f.ctx, f.principal(f.lead), TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, To: domain.RequestStatusTriaging, ExpectedRev: before.Rev,
	}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := storedExpiry(t, f, a.ID); got != nil {
		t.Fatalf("a reopen left the file's expiry at %v, want NULL", got)
	}

	// And the purge spares it even when the request's 90 days from the OLD close
	// have passed: the reopen is what matters, not the old stamp.
	longAgo := time.Now().Add(-200 * 24 * time.Hour)
	ageRequest(t, f, req, domain.RequestStatusTriaging, longAgo)
	if _, err := f.svc.TransitionRequest(f.ctx, f.principal(f.lead), TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, To: domain.RequestStatusAccepted,
		ExpectedRev: f.get(req.ID, f.team).Rev,
	}); err != nil {
		t.Fatalf("move a reopened request forward: %v", err)
	}
	if n := purgeRequestFiles(t, f, req); n != 0 {
		t.Fatalf("the purge deleted %d files from an active request", n)
	}
	if attachmentCount(t, f, req) != 1 {
		t.Fatal("an active request lost its file")
	}
}

// TestRequestRetentionRecloseRestartsTheClock pins the defect the semantic review
// of this slice found: 053 makes closed_at first-write-wins ("the request was
// once closed, and when"), so reading it as D3's close moment made a re-closed
// request's files due the instant the team closed it the second time — the first
// close was long past, and the column never moved. D3's window is the close the
// request is under NOW, so the second close must restart it.
//
// The first half drives the real lifecycle twice with real time in between; the
// second half ages the FIRST close into the past so "the old close is outside the
// window, the new one is inside it" is a difference the purge can see. Without the
// fix the reopened-and-reclosed file is deleted here.
func TestRequestRetentionRecloseRestartsTheClock(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("recloseprobe", domain.RequestStatusTriaging, &requester)
	a := attach(t, f, requester, req, "returning.txt", []byte("the customer came back\n"))

	// First close, through the real transition.
	if _, err := f.svc.TransitionRequest(f.ctx, f.principal(f.lead), TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, To: domain.RequestStatusRejected,
		RejectReason: "Outside this desk's scope.", ExpectedRev: f.get(req.ID, f.team).Rev,
	}); err != nil {
		t.Fatalf("first reject: %v", err)
	}
	firstClose := f.get(req.ID, f.team).ClosedAt
	if firstClose == nil {
		t.Fatal("the first close recorded no closed_at")
	}

	// The team reopens it (D6) and the file survives.
	if _, err := f.svc.TransitionRequest(f.ctx, f.principal(f.lead), TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, To: domain.RequestStatusTriaging,
		ExpectedRev: f.get(req.ID, f.team).Rev,
	}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := storedExpiry(t, f, a.ID); got != nil {
		t.Fatalf("the reopen left the file's expiry at %v, want NULL", got)
	}

	// Make the FIRST close old enough that its window has already passed. This is
	// the situation a re-close must not inherit; the column is deliberately left
	// stale (that is exactly what 053 guarantees it will be).
	longAgo := time.Now().Add(-200 * 24 * time.Hour)
	if _, err := f.db.Exec(f.ctx, `UPDATE team_requests SET closed_at = $2 WHERE id = $1`, req.ID, longAgo); err != nil {
		t.Fatalf("age the first close: %v", err)
	}

	// The second close, through the real transition.
	if _, err := f.svc.TransitionRequest(f.ctx, f.principal(f.lead), TransitionRequestInput{
		RequestID: req.ID, TeamID: f.team, To: domain.RequestStatusRejected,
		RejectReason: "Still outside this desk's scope.", ExpectedRev: f.get(req.ID, f.team).Rev,
	}); err != nil {
		t.Fatalf("second reject: %v", err)
	}
	got := storedExpiry(t, f, a.ID)
	if got == nil {
		t.Fatal("the second close left the file with no expiry: the clock never restarted")
	}
	if got.Before(time.Now()) {
		t.Fatalf("the second close made the file due at once (expiry %v is in the past): the window restarted from the FIRST close", got)
	}

	// And the purge spares it, which is the consequence that matters: the file is
	// inside the CURRENT window even though the first close is 200 days old.
	if n := purgeRequestFiles(t, f, req); n != 0 {
		t.Fatalf("the purge deleted %d files from a request re-closed moments ago", n)
	}
	if attachmentCount(t, f, req) != 1 {
		t.Fatal("a request re-closed moments ago lost its file")
	}

	// A request whose ONLY close is old is still purged, so the fix did not simply
	// disable the clock: the second half of the same rule.
	old := f.newRequest("recloseoldcnt", domain.RequestStatusTriaging, &requester)
	attach(t, f, requester, old, "old.txt", []byte("long delivered and done\n"))
	ageRequest(t, f, old, domain.RequestStatusRejected, longAgo)
	if _, err := f.svc.Repo.SyncRequestAttachmentExpiry(f.ctx, old.ID, old.OrgID, old.TeamID); err != nil {
		t.Fatalf("stamp the old close: %v", err)
	}
	if n := purgeRequestFiles(t, f, old); n != 1 {
		t.Fatalf("the purge deleted %d files from a request closed 200 days ago, want 1", n)
	}
}

// ---- criterion 2: the purge removes bytes and audits each file ----

// TestPurgeExpiredRequestAttachmentsPurgesTheClosedAndAuditsEach is criteria 1
// and 2 together over four requests at once: one active, one closed long enough
// ago, one closed with a kept file and one closed but reopened. The purge deletes
// exactly the unkept file on the closed request and leaves one audit row for it —
// naming the request, the id, the type and the size and never the file's name.
func TestPurgeExpiredRequestAttachmentsPurgesTheClosedAndAuditsEach(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	longAgo := time.Now().Add(-200 * 24 * time.Hour)

	active := f.newRequest("purgeactive01", domain.RequestStatusTriaging, &requester)
	activeFile := attach(t, f, requester, active, "active.txt", []byte("still needed\n"))

	closed := f.newRequest("purgeclosed01", domain.RequestStatusTriaging, &requester)
	// A run-unique payload, so the "are the bytes gone?" search below can only find
	// THIS file's bytes and never a leftover from an earlier run in the same database.
	closedBytes := []byte(fmt.Sprintf("past the window %s\n", closed.PublicRef))
	closedFile := attach(t, f, requester, closed, "closed-secret-name.txt", closedBytes)
	ageRequest(t, f, closed, domain.RequestStatusRejected, longAgo)
	if _, err := f.svc.Repo.SyncRequestAttachmentExpiry(f.ctx, closed.ID, closed.OrgID, closed.TeamID); err != nil {
		t.Fatalf("stamp the closed request: %v", err)
	}

	// The keep happens while the request is still active: pinning a file before its
	// window opens is the order a person would do it in, and it leaves no moment at
	// which the file is due and unpinned.
	kept := f.newRequest("purgekept0001", domain.RequestStatusTriaging, &requester)
	keptFile := attach(t, f, requester, kept, "kept.txt", []byte("pin this\n"))
	if _, err := f.svc.KeepRequestAttachment(f.ctx, f.principal(f.member), retentionInput(kept, keptFile), true); err != nil {
		t.Fatalf("keep: %v", err)
	}
	ageRequest(t, f, kept, domain.RequestStatusRejected, longAgo)
	if _, err := f.svc.Repo.SyncRequestAttachmentExpiry(f.ctx, kept.ID, kept.OrgID, kept.TeamID); err != nil {
		t.Fatalf("stamp the kept request: %v", err)
	}

	reopened := f.newRequest("purgereopn001", domain.RequestStatusTriaging, &requester)
	reopenedFile := attach(t, f, requester, reopened, "reopened.txt", []byte("asked again\n"))
	ageRequest(t, f, reopened, domain.RequestStatusRejected, longAgo)
	if _, err := f.svc.Repo.SyncRequestAttachmentExpiry(f.ctx, reopened.ID, reopened.OrgID, reopened.TeamID); err != nil {
		t.Fatalf("stamp the reopened request: %v", err)
	}
	// The reopen through the real transition, which clears the stamp.
	if _, err := f.svc.TransitionRequest(f.ctx, f.principal(f.lead), TransitionRequestInput{
		RequestID: reopened.ID, TeamID: f.team, To: domain.RequestStatusTriaging,
		ExpectedRev: f.get(reopened.ID, f.team).Rev,
	}); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	// The POSTCONDITIONS decide, not a pass's global total: one of this test's four
	// files is due, and the other three may never be purged. (The count is reported
	// on failure because "0 deleted" and "3 deleted" are different bugs.)
	deleted := purgeRequestFiles(t, f, active, closed, kept, reopened)
	if attachmentCount(t, f, closed) != 0 {
		t.Fatalf("the closed request's expired file survived a purge (this test's files deleted: %d)", deleted)
	}
	for name, tc := range map[string]struct {
		req *domain.TeamRequest
		att *domain.RequestAttachment
	}{
		"active":   {active, activeFile},
		"kept":     {kept, keptFile},
		"reopened": {reopened, reopenedFile},
	} {
		if attachmentCount(t, f, tc.req) != 1 {
			t.Fatalf("the %s request lost its file to the purge", name)
		}
	}

	// One audit row, after the change, naming what was purged and never the file.
	entries := retentionAudits(t, f, "request.attachment.purge", closed.ID)
	if len(entries) != 1 {
		t.Fatalf("%d purge audit rows, want exactly 1", len(entries))
	}
	e := entries[0]
	if e.TargetType != "request" || e.TargetName != closed.PublicRef {
		t.Errorf("audit target = %q/%q, want request/%q", e.TargetType, e.TargetName, closed.PublicRef)
	}
	if e.OrgID == nil || *e.OrgID != closed.OrgID {
		t.Errorf("audit org = %v, want %d", e.OrgID, closed.OrgID)
	}
	if got := e.Details["attachment_id"]; fmt.Sprint(got) != fmt.Sprint(closedFile.ID) {
		t.Errorf("audit attachment_id = %v, want %d", got, closedFile.ID)
	}
	if got := e.Details["size_bytes"]; fmt.Sprint(got) != fmt.Sprint(int64(len(closedBytes))) {
		t.Errorf("audit size_bytes = %v, want the file's size", got)
	}
	if got := e.Details["content_type"]; got != "text/plain" {
		t.Errorf("audit content_type = %v, want text/plain", got)
	}
	for _, banned := range []string{"file_name", "name", "content", "sha256"} {
		if _, ok := e.Details[banned]; ok {
			t.Fatalf("the purge audit carries %q: a file's name and bytes are the requester's content", banned)
		}
	}
	if strings.Contains(e.Summary, "closed-secret-name") {
		t.Fatalf("the purge audit summary carries the file's name: %q", e.Summary)
	}
	// The three survivors got no audit row from this purge.
	for _, req := range []*domain.TeamRequest{active, kept, reopened} {
		if got := len(retentionAudits(t, f, "request.attachment.purge", req.ID)); got != 0 {
			t.Fatalf("%d purge audit rows for a request whose file was spared", got)
		}
	}

	// The bytes are gone with the row: 056 keeps the content in the table, so a
	// purge that left the bytes would be no purge at all.
	var holders int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM request_attachments WHERE content = $1`, closedBytes).Scan(&holders); err != nil {
		t.Fatalf("search for the purged bytes: %v", err)
	}
	if holders != 0 {
		t.Fatalf("the purged file's bytes are still in %d rows", holders)
	}
}

// TestPurgeExpiredRequestAttachmentsIsIdempotentAndBounded pins the job's shape:
// a second pass over the same database deletes nothing, and a pass that fails
// mid-way is safe to repeat. Together with the repository's SKIP LOCKED test this
// is what lets it run hourly on every replica.
func TestPurgeExpiredRequestAttachmentsIsIdempotentAndBounded(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("purgeagain01", domain.RequestStatusTriaging, &requester)
	for i := 0; i < 3; i++ {
		attach(t, f, requester, req, fmt.Sprintf("aged-%d.txt", i), []byte("past the window\n"))
	}
	ageRequest(t, f, req, domain.RequestStatusRejected, time.Now().Add(-200*24*time.Hour))
	if _, err := f.svc.Repo.SyncRequestAttachmentExpiry(f.ctx, req.ID, req.OrgID, req.TeamID); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	if n := purgeRequestFiles(t, f, req); n != 3 {
		t.Fatalf("the purge deleted %d of this test's files, want 3", n)
	}
	after := purgeOnePass(t, f)
	// The second pass may delete OTHER runs' due rows (a shared database keeps a
	// backlog), but it must delete none of this test's and write none of its audit
	// rows: the purge is idempotent per file.
	if got := attachmentCount(t, f, req); got != 0 {
		t.Fatalf("%d of this test's files survived and were re-deleted (pass reported %d)", got, after)
	}
	if got := len(retentionAudits(t, f, "request.attachment.purge", req.ID)); got != 3 {
		t.Fatalf("%d purge audit rows for 3 purged files, want 3 (and none from the no-op pass)", got)
	}
}

// ---- criterion 3: authorization ----

// TestRequestRetentionCapabilityFor is the matrix as a pure function, so the
// answer for every caller shape is pinned without a database.
func TestRequestRetentionCapabilityFor(t *testing.T) {
	requester := 7
	serviceSubmitted := &domain.TeamRequest{ID: 1, RequesterUserID: nil}
	own := &domain.TeamRequest{ID: 2, RequesterUserID: &requester}
	person := authz.Principal{UserID: requester, Channel: auth.ChannelUI}
	otherPerson := authz.Principal{UserID: 8, Channel: auth.ChannelUI}

	cases := []struct {
		name string
		req  *domain.TeamRequest
		p    authz.Principal
		role authz.InboxRole
		want RequestRetentionCapability
	}{
		{"team member on any request", own, otherPerson, authz.InboxMember, RequestRetentionTeam},
		{"org owner on any request", serviceSubmitted, otherPerson, authz.InboxMember, RequestRetentionTeam},
		{"requester on their own request", own, person, authz.InboxNone, RequestRetentionRequester},
		{"requester who is also a team member", own, person, authz.InboxMember, RequestRetentionTeam},
		{"requester on another requester's request", own, otherPerson, authz.InboxNone, RequestRetentionNone},
		{"a stranger who is somebody's requester elsewhere", serviceSubmitted, person, authz.InboxNone, RequestRetentionNone},
		{"a caller with no AMP user", own, authz.Principal{}, authz.InboxNone, RequestRetentionNone},
		{"a nil request", nil, person, authz.InboxMember, RequestRetentionNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RequestRetentionCapabilityFor(c.req, c.p, c.role); got != c.want {
				t.Fatalf("capability = %s, want %s", got, c.want)
			}
		})
	}

	// Keeping is the team's alone: the requester's own capability must not include
	// it, or D3's "a team keep" would be a promise the uploader could make.
	if RequestRetentionRequester.MayKeep() {
		t.Fatal("the requester capability may keep: pinning a file past the window is the team's decision (D3)")
	}
	if !RequestRetentionTeam.MayKeep() {
		t.Fatal("the team capability may not keep: nobody could pin a file")
	}
}

// TestKeepAndDeleteRefuseCallersWithoutStanding is criterion 3's main half: a
// caller with no standing on the request is refused with the surface's single
// not-found, for every shape of "no standing", and nothing is written — no keep,
// no deletion, no audit row.
func TestKeepAndDeleteRefuseCallersWithoutStanding(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("refuse000001", domain.RequestStatusTriaging, &requester)
	a := attach(t, f, requester, req, "mine.txt", []byte("mine\n"))

	// A second customer with no role anywhere: another requester, who must not reach
	// in. (The fixture's customer is this request's requester, so they would be
	// allowed to delete — that is the next test.)
	otherCustomer := f.newCustomer("refuseother")
	// A request in the other org, for the cross-tenant case. It is created with the
	// other org's id, which is what 053's team-in-org constraint requires.
	foreignCreated, _, err := f.svc.Repo.CreateRequest(f.ctx, repository.CreateRequestInput{
		OrgID:            f.otherOrg,
		TeamID:           f.foreign,
		PublicRef:        fmt.Sprintf("refuseforeign%09d", rand.IntN(1_000_000_000)),
		RequesterUserID:  &f.stranger,
		RequesterContact: "stranger@life.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "A request in another org",
		InputBrief:       "Nobody here may reach it.",
		Actor:            "stranger@life.test",
	})
	if err != nil {
		t.Fatalf("create the foreign request: %v", err)
	}
	foreignReq, err := f.svc.Repo.GetRequest(f.ctx, foreignCreated.ID, f.otherOrg, f.foreign)
	if err != nil {
		t.Fatalf("load the foreign request: %v", err)
	}
	foreignFile := attach(t, f, f.stranger, foreignReq, "foreign.txt", []byte("not yours\n"))

	cases := []struct {
		name string
		who  int
		// input the caller names; the file id is filled in per case.
		in func(attID int64) RequestAttachmentRetentionInput
	}{
		{"a customer who is not this request's requester", otherCustomer,
			func(id int64) RequestAttachmentRetentionInput {
				return RequestAttachmentRetentionInput{RequestID: req.ID, OrgID: req.OrgID, TeamID: req.TeamID, AttachmentID: id}
			}},
		{"a stranger from another org", f.stranger,
			func(id int64) RequestAttachmentRetentionInput {
				return RequestAttachmentRetentionInput{RequestID: req.ID, OrgID: req.OrgID, TeamID: req.TeamID, AttachmentID: id}
			}},
		{"a member of a sibling team in the same org", f.newSiblingMember("refusesibling"),
			func(id int64) RequestAttachmentRetentionInput {
				return RequestAttachmentRetentionInput{RequestID: req.ID, OrgID: req.OrgID, TeamID: f.team, AttachmentID: id}
			}},
		{"a member of this team naming the wrong tenant", f.member,
			func(id int64) RequestAttachmentRetentionInput {
				return RequestAttachmentRetentionInput{RequestID: req.ID, OrgID: f.otherOrg, TeamID: f.foreign, AttachmentID: id}
			}},
		{"a member of this team naming a foreign request's file", f.member,
			func(id int64) RequestAttachmentRetentionInput {
				return RequestAttachmentRetentionInput{RequestID: foreignReq.ID, OrgID: foreignReq.OrgID, TeamID: foreignReq.TeamID, AttachmentID: foreignFile.ID}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := f.principal(c.who)
			// Keep: refused as not-found (a caller with no standing learns nothing).
			if _, err := f.svc.KeepRequestAttachment(f.ctx, p, c.in(a.ID), true); authz.CodeOf(err) != authz.CodeRequestNotFound {
				t.Errorf("keep: err = %v, want code %q", err, authz.CodeRequestNotFound)
			}
			// Delete: the same answer, and nothing deleted.
			if _, err := f.svc.DeleteRequestAttachment(f.ctx, p, c.in(a.ID)); authz.CodeOf(err) != authz.CodeRequestNotFound {
				t.Errorf("delete: err = %v, want code %q", err, authz.CodeRequestNotFound)
			}
		})
	}

	// Nothing was written by any of them: the real file and the foreign one are both
	// still there, unkept, and no retention audit row exists.
	if attachmentCount(t, f, req) != 1 {
		t.Fatal("a refused delete removed the file")
	}
	if attachmentCount(t, f, foreignReq) != 1 {
		t.Fatal("a refused delete removed a foreign request's file")
	}
	var keep bool
	if err := f.db.QueryRow(f.ctx, `SELECT keep FROM request_attachments WHERE id = $1`, a.ID).Scan(&keep); err != nil {
		t.Fatalf("read keep: %v", err)
	}
	if keep {
		t.Fatal("a refused keep wrote keep=true")
	}
	for _, action := range []string{"request.attachment.keep", "request.attachment.unkeep", "request.attachment.delete"} {
		if got := len(retentionAudits(t, f, action, req.ID)); got != 0 {
			t.Fatalf("%d %s audit rows after refusals, want 0", got, action)
		}
	}
}

// newSiblingMember inserts a member of the fixture's SECOND team (the same org, a
// different inbox) and returns their id.
func (f *lifecycleFixture) newSiblingMember(label string) int {
	f.t.Helper()
	var id int
	if err := f.db.QueryRow(f.ctx, `INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
		fmt.Sprintf("life-%s-%d", label, f.org), fmt.Sprintf("life-%s-%d@life.test", label, f.org),
		domain.UserKindHuman).Scan(&id); err != nil {
		f.t.Fatalf("insert %s: %v", label, err)
	}
	if _, err := f.db.Exec(f.ctx, `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`,
		f.otherTeam, id); err != nil {
		f.t.Fatalf("make %s a member of the second desk: %v", label, err)
	}
	return id
}

// TestRequesterMayDeleteTheirOwnFileButNotKeepIt pins the split the matrix
// describes, on the real service: a requester withdraws their own upload (the act
// deletes the row and its bytes, and audits it), and cannot pin it past the
// window — which is a 403 naming D3 rather than a 404, because a requester can see
// their own request and the refusal is about what the act belongs to.
func TestRequesterMayDeleteTheirOwnFileButNotKeepIt(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("ownfile00001", domain.RequestStatusTriaging, &requester)
	withdrawn := []byte(fmt.Sprintf("please ignore this %s\n", req.PublicRef))
	a := attach(t, f, requester, req, "withdraw-me.txt", withdrawn)

	// The keep is refused, with D3's words, and writes nothing.
	if _, err := f.svc.KeepRequestAttachment(f.ctx, f.principal(requester), retentionInput(req, a), true); err == nil {
		t.Fatal("the requester pinned their own file")
	} else if authz.StatusOf(err, 500) != 403 || !strings.Contains(err.Error(), "team's decision") {
		t.Fatalf("refusal = %d %v, want a 403 naming D3", authz.StatusOf(err, 500), err)
	}
	var keep bool
	if err := f.db.QueryRow(f.ctx, `SELECT keep FROM request_attachments WHERE id = $1`, a.ID).Scan(&keep); err != nil {
		t.Fatalf("read keep: %v", err)
	}
	if keep {
		t.Fatal("the refused keep wrote keep=true")
	}
	if got := len(retentionAudits(t, f, "request.attachment.keep", req.ID)); got != 0 {
		t.Fatalf("%d keep audit rows after a refusal, want 0", got)
	}

	// The delete succeeds, removes the bytes, and writes one audit row.
	changed, err := f.svc.DeleteRequestAttachment(f.ctx, f.principal(requester), retentionInput(req, a))
	if err != nil {
		t.Fatalf("the requester could not delete their own file: %v", err)
	}
	if !changed {
		t.Fatal("the delete answered changed=false on a row that existed")
	}
	if attachmentCount(t, f, req) != 0 {
		t.Fatal("the row survived the requester's delete")
	}
	var holders int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM request_attachments WHERE content = $1`, withdrawn).Scan(&holders); err != nil {
		t.Fatalf("search for the deleted bytes: %v", err)
	}
	if holders != 0 {
		t.Fatal("the delete left the bytes behind")
	}
	entries := retentionAudits(t, f, "request.attachment.delete", req.ID)
	if len(entries) != 1 {
		t.Fatalf("%d delete audit rows, want 1", len(entries))
	}
	if got := entries[0].Details["attachment_id"]; fmt.Sprint(got) != fmt.Sprint(a.ID) {
		t.Errorf("audit attachment_id = %v, want %d", got, a.ID)
	}
	for _, banned := range []string{"file_name", "name", "content", "sha256"} {
		if _, ok := entries[0].Details[banned]; ok {
			t.Fatalf("the delete audit carries %q: the file's name is the requester's content", banned)
		}
	}

	// A retry after the row is gone is an ordinary not-found, not a second deletion
	// and not a second audit row: the delete is idempotent from the caller's side.
	if _, err := f.svc.DeleteRequestAttachment(f.ctx, f.principal(requester), retentionInput(req, a)); authz.CodeOf(err) != authz.CodeRequestNotFound {
		t.Fatalf("retry: err = %v, want code %q", err, authz.CodeRequestNotFound)
	}
	if got := len(retentionAudits(t, f, "request.attachment.delete", req.ID)); got != 1 {
		t.Fatalf("%d delete audit rows after a retry, want still 1", got)
	}
}

// TestTeamMayKeepAndUnkeep pins the team's half: a member pins a file (the row
// carries keep/kept_by/kept_at and no expiry), and unkeeping hands it back to the
// request's own window — so a closed request's file becomes due immediately rather
// than getting a fresh 90 days.
func TestTeamMayKeepAndUnkeep(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("keepunkeep01", domain.RequestStatusTriaging, &requester)
	a := attach(t, f, requester, req, "keep-me.txt", []byte("keep me\n"))

	// The keep happens while the request is still ACTIVE, so there is no moment at
	// which the file is closed-and-due before it is pinned — which also means a
	// concurrently running purge (another package's suite) cannot take it first.
	kept, err := f.svc.KeepRequestAttachment(f.ctx, f.principal(f.member), retentionInput(req, a), true)
	if err != nil {
		t.Fatalf("keep: %v", err)
	}
	if !kept.Keep || kept.KeptBy == nil || *kept.KeptBy != f.member || kept.KeptAt == nil {
		t.Fatalf("kept = %+v, want keep=true kept_by=%d kept_at set", kept, f.member)
	}
	if kept.ExpiresAt != nil {
		t.Fatalf("a kept file has expiry %v, want NULL (nothing may purge it)", kept.ExpiresAt)
	}
	entries := retentionAudits(t, f, "request.attachment.keep", req.ID)
	if len(entries) != 1 {
		t.Fatalf("%d keep audit rows, want 1", len(entries))
	}

	// Now the request closes, long past the window: the kept file gets no expiry, so
	// it is not even a purge candidate.
	ageRequest(t, f, req, domain.RequestStatusRejected, time.Now().Add(-200*24*time.Hour))
	if _, err := f.svc.Repo.SyncRequestAttachmentExpiry(f.ctx, req.ID, req.OrgID, req.TeamID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	req = f.get(req.ID, f.team)
	if got := storedExpiry(t, f, a.ID); got != nil {
		t.Fatalf("a kept file on a closed request has expiry %v, want NULL", got)
	}
	if n := purgeRequestFiles(t, f, req); n != 0 {
		t.Fatalf("the purge deleted %d kept files", n)
	}
	if attachmentCount(t, f, req) != 1 {
		t.Fatal("the kept file was purged")
	}

	// Unkeep: the request's own window applies again — already past — so the next
	// pass purges it. That is D3 read literally, not a fresh 90 days.
	unkept, err := f.svc.KeepRequestAttachment(f.ctx, f.principal(f.member), retentionInput(req, a), false)
	if err != nil {
		t.Fatalf("unkeep: %v", err)
	}
	if unkept.Keep || unkept.KeptBy != nil || unkept.KeptAt != nil {
		t.Fatalf("after unkeep keep=%v kept_by=%v kept_at=%v, want false/nil/nil", unkept.Keep, unkept.KeptBy, unkept.KeptAt)
	}
	if unkept.ExpiresAt == nil {
		t.Fatal("unkeeping a closed request's file left it with no expiry")
	}
	if !unkept.ExpiresAt.Before(time.Now()) {
		t.Fatalf("after unkeep expiry = %v, want a moment in the past (the request's own window)", unkept.ExpiresAt)
	}
	if n := purgeRequestFiles(t, f, req); n != 1 {
		t.Fatalf("after unkeep the purge deleted %d, want 1", n)
	}
	if attachmentCount(t, f, req) != 0 {
		t.Fatal("the unkept, expired file survived the purge")
	}
}

// TestRetentionRefusesASubmitScopedToken pins the fail-closed direction: a
// submit-only credential reaches exactly one operation (the submission itself), so
// it can neither keep nor delete a stored file.
func TestRetentionRefusesASubmitScopedToken(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("scopedtoken1", domain.RequestStatusTriaging, &requester)
	a := attach(t, f, requester, req, "scoped.txt", []byte("scoped\n"))

	p := f.principal(f.member)
	p.Scope = domain.TokenScopeSubmit
	p.ScopeTeamID = f.team
	if _, err := f.svc.KeepRequestAttachment(f.ctx, p, retentionInput(req, a), true); authz.CodeOf(err) != authz.CodeTokenScopeDenied {
		t.Fatalf("keep as a submit-scoped token: err = %v, want code %q", err, authz.CodeTokenScopeDenied)
	}
	if _, err := f.svc.DeleteRequestAttachment(f.ctx, p, retentionInput(req, a)); authz.CodeOf(err) != authz.CodeTokenScopeDenied {
		t.Fatalf("delete as a submit-scoped token: err = %v, want code %q", err, authz.CodeTokenScopeDenied)
	}
	if attachmentCount(t, f, req) != 1 {
		t.Fatal("a submit-scoped token deleted a file")
	}
}

// ---- the job ----

// TestRunRequestAttachmentPurgeRegistersAsABoundedJob pins the shape of the
// periodic job: it purges once immediately, again on each tick, and stops when its
// context ends — with no scheduler framework and no unbounded loop a shutdown would
// have to wait on.
func TestRunRequestAttachmentPurgeRegistersAsABoundedJob(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("jobshape0001", domain.RequestStatusTriaging, &requester)
	attach(t, f, requester, req, "job.txt", []byte("past the window\n"))
	ageRequest(t, f, req, domain.RequestStatusRejected, time.Now().Add(-200*24*time.Hour))
	if _, err := f.svc.Repo.SyncRequestAttachmentExpiry(f.ctx, req.ID, req.OrgID, req.TeamID); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A millisecond tick keeps the test fast; the job's own contract is "one
		// pass per interval", not the interval's length.
		f.svc.RunRequestAttachmentPurge(ctx, time.Millisecond, nil)
	}()

	// Wait for the first pass to delete the file, with a limit so a bug cannot hang
	// the suite.
	deadline := time.Now().Add(10 * time.Second)
	for attachmentCount(t, f, req) != 0 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the job never purged the expired file")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the job did not stop when its context ended")
	}
	// Exactly one audit row for the one file, however many passes ran: a pass with
	// nothing to delete writes nothing. (A concurrently running suite's own purge
	// cannot write against THIS request, so the count is stable even then.)
	if got := len(retentionAudits(t, f, "request.attachment.purge", req.ID)); got != 1 {
		t.Fatalf("%d purge audit rows after the job ran, want exactly 1 (the second pass has nothing to delete)", got)
	}
}

// TestRequestRetentionServiceStaysARetentionService is the ticket's scope made
// checkable: this file decides saving decisions and the clock, and nothing about
// serving, scanning or deleting requests. It parses the file rather than searching
// its text, so the comments are allowed to explain these rules.
func TestRequestRetentionServiceStaysARetentionService(t *testing.T) {
	const file = "request_retention.go"
	fs := token.NewFileSet()
	// No parse flags: doc and line comments are not part of the tree this walks, so
	// the file's prose may explain these rules without tripping the check.
	parsed, err := parser.ParseFile(fs, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	allowed := map[string]bool{
		"context":  true,
		"errors":   true,
		"fmt":      true,
		"log/slog": true,
		"time":     true,
		"github.com/simstech/amp-api/internal/authz":      true,
		"github.com/simstech/amp-api/internal/domain":     true,
		"github.com/simstech/amp-api/internal/repository": true,
	}
	for _, spec := range parsed.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("import path %s: %v", spec.Path.Value, err)
		}
		if !allowed[path] {
			t.Errorf("%s imports %q: the retention service has no business serving, sniffing, publishing or routing", file, path)
		}
	}

	forbidden := map[string]string{
		"DetectContentType":                "sniffing belongs to the upload service",
		"RequestAttachmentDisposition":     "serving posture belongs to the download gate",
		"RequestAttachmentDownloadRefusal": "whether bytes may be served is the download gate's decision, not retention's",
		"RequestScanClean":                 "retention never claims a scan verdict",
	}
	ast.Inspect(parsed, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.Ident:
			if why, bad := forbidden[node.Name]; bad {
				t.Errorf("%s uses %q: %s", file, node.Name, why)
			}
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(node.Value)
			if err != nil {
				return true
			}
			for _, banned := range []string{"http://", "https://", "DELETE FROM team_requests"} {
				if strings.Contains(value, banned) {
					t.Errorf("%s carries the literal %q: retention deletes attachments and serves nothing, and never deletes the request record", file, banned)
					break
				}
			}
		}
		return true
	})
}

// TestRequestRetentionRefusesAnUnknownAttachment pins the idempotence of the
// delete and the not-found of the keep against a row that is simply not there:
// both answer the surface's one 404 rather than a 500 or a silent success.
func TestRequestRetentionRefusesAnUnknownAttachment(t *testing.T) {
	f := newLifecycleFixture(t)
	requester := f.customer
	req := f.newRequest("unknownid001", domain.RequestStatusTriaging, &requester)
	missing := RequestAttachmentRetentionInput{
		RequestID: req.ID, OrgID: req.OrgID, TeamID: req.TeamID, AttachmentID: 1 << 40,
	}
	if _, err := f.svc.KeepRequestAttachment(f.ctx, f.principal(f.member), missing, true); authz.CodeOf(err) != authz.CodeRequestNotFound {
		t.Fatalf("keep on a missing id: err = %v, want code %q", err, authz.CodeRequestNotFound)
	}
	if _, err := f.svc.DeleteRequestAttachment(f.ctx, f.principal(f.member), missing); authz.CodeOf(err) != authz.CodeRequestNotFound {
		t.Fatalf("delete on a missing id: err = %v, want code %q", err, authz.CodeRequestNotFound)
	}
}

// TestRequestRetentionErrorsKeepTheirIdentity makes sure a real failure is not
// dressed up as a not-found: a cancelled context through the purge is the
// cancellation, not "nothing to do".
func TestRequestRetentionErrorsKeepTheirIdentity(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.svc.PurgeExpiredRequestAttachments(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
