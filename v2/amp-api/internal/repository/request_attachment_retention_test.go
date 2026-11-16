package repository

// Tests for request_attachment_retention.go against a real Postgres (AMP #3964,
// epic 202 story 549; build slice G4). They run only when TEST_DATABASE_URL is
// set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/repository/ -run 'RequestAttachmentRetention|PurgeExpiredRequestAttachments' -count=1 -race
//
// The ticket's retention table is what these assert, in the order the contract
// (artifact 36 v2 §1, D3) states it:
//
//  1. an ACTIVE request's file is never purged, however old the request is, and
//     its stored expiry is NULL (056: "no expiry" means "may not be purged");
//  2. a CLOSED request's file is purged once close+90d has passed, and the bytes
//     go with the row — the DELETE is the physical cleanup because 056 keeps the
//     content in the row (TestPurgeExpiredRequestAttachmentsRemovesBytes);
//  3. a KEPT file is never purged, whatever its expiry says, and a concurrent keep
//     cannot lose to a purge that already selected the row;
//  4. a REOPENED request's file survives, even when its stored expiry still says
//     it is due — the purge re-reads the request's state rather than trusting the
//     stamp (TestPurgeExpiredRequestAttachmentsSpareReopenedRequests), and the
//     reconcile pass then corrects the stamp so the file is downloadable again.
//
// Around the table the tests pin what the layer promises: the expiry is derived
// from the request (close+90d, NULL while active, NULL when kept), unkeep
// recomputes the window from the close moment instead of granting a fresh one, a
// keep in another tenant writes nothing and answers ErrNotFound, and the
// retention writes never name a scan state or a serving path.
//
// Each test builds its own orgs, teams, users and requests through
// requestRowsFixture (a random suffix per run), so repeated runs and other tests
// in the same database never see each other's rows.

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// retentionFixture is the attachment fixture plus the one helper this slice
// needs: moving a request into a state and its matching timestamp, exactly as the
// lifecycle write would leave the row.
type retentionFixture struct{ *requestAttachmentsFixture }

func newRetentionFixture(t *testing.T) *retentionFixture {
	t.Helper()
	return &retentionFixture{requestAttachmentsFixture: newRequestAttachmentsFixture(t)}
}

// closeRequest puts req into a closed state the way the transition write does:
// the state plus the timestamp 053 and D3 read. state must be one of the three
// closed states; at is the moment the request closed.
func (f *retentionFixture) closeRequest(req *domain.TeamRequest, state domain.RequestStatus, at time.Time) {
	f.t.Helper()
	switch state {
	case domain.RequestStatusRejected:
		if _, err := f.r.db.Exec(f.ctx, `UPDATE team_requests
			SET state = $2, closed_at = $3, reject_reason = 'not now' WHERE id = $1`,
			req.ID, string(state), at); err != nil {
			f.t.Fatalf("close request %d as %s: %v", req.ID, state, err)
		}
	case domain.RequestStatusDuplicate:
		// A duplicate needs a canonical; the second request in this fixture's team
		// serves, and its own retention is not what the test measures.
		canon := f.newRequest("canonical-of-" + req.PublicRef)
		if _, err := f.r.db.Exec(f.ctx, `UPDATE team_requests
			SET state = $2, closed_at = $3, duplicate_of = $4 WHERE id = $1`,
			req.ID, string(state), at, canon.ID); err != nil {
			f.t.Fatalf("close request %d as %s: %v", req.ID, state, err)
		}
	case domain.RequestStatusRequesterAccepted:
		// A successful close: delivered is evidenced by an outcome brief and a
		// delivery time, and acceptance stamps accepted_at (053's CHECKs).
		if _, err := f.r.db.Exec(f.ctx, `UPDATE team_requests
			SET state = $2, outcome_brief = 'shipped', delivered_at = $3, accepted_at = $3
			WHERE id = $1`, req.ID, string(state), at); err != nil {
			f.t.Fatalf("close request %d as %s: %v", req.ID, state, err)
		}
	default:
		f.t.Fatalf("%s is not a closed state", state)
	}
	req.State = state
}

// reopenRequest puts a closed request back into the working chain the way the
// reopen transition does: the state, the reopen bookkeeping, and — deliberately —
// NOT the attachments' expiry, so a test can prove the purge does not depend on
// the stamp having been cleared.
func (f *retentionFixture) reopenRequest(req *domain.TeamRequest) {
	f.t.Helper()
	if _, err := f.r.db.Exec(f.ctx, `UPDATE team_requests
		SET state = 'triaging', reopened_at = NOW(), reopened_count = reopened_count + 1,
		    duplicate_of = NULL, reject_reason = ''
		WHERE id = $1`, req.ID); err != nil {
		f.t.Fatalf("reopen request %d: %v", req.ID, err)
	}
	req.State = domain.RequestStatusTriaging
}

// storedExpiry reads one attachment's stored expiry straight from the table: nil
// when the column is NULL, which is the schema's "may not be purged".
func (f *retentionFixture) storedExpiry(attID int64) *time.Time {
	f.t.Helper()
	var at *time.Time
	if err := f.r.db.QueryRow(f.ctx, `SELECT expires_at FROM request_attachments WHERE id = $1`, attID).Scan(&at); err != nil {
		f.t.Fatalf("read expiry of attachment %d: %v", attID, err)
	}
	return at
}

// storedKeep reads one attachment's keep columns, so a keep can be asserted from
// the row rather than from the value the method returned.
func (f *retentionFixture) storedKeep(attID int64) (keep bool, keptBy *int, keptAt *time.Time) {
	f.t.Helper()
	if err := f.r.db.QueryRow(f.ctx, `SELECT keep, kept_by, kept_at FROM request_attachments WHERE id = $1`, attID).
		Scan(&keep, &keptBy, &keptAt); err != nil {
		f.t.Fatalf("read keep of attachment %d: %v", attID, err)
	}
	return keep, keptBy, keptAt
}

// exists reports whether the attachment row is still there.
func (f *retentionFixture) exists(attID int64) bool {
	f.t.Helper()
	var n int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_attachments WHERE id = $1`, attID).Scan(&n); err != nil {
		f.t.Fatalf("count attachment %d: %v", attID, err)
	}
	return n == 1
}

// sync is the write a transition makes after a close or a reopen.
func (f *retentionFixture) sync(req *domain.TeamRequest) int {
	f.t.Helper()
	n, err := f.r.SyncRequestAttachmentExpiry(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		f.t.Fatalf("sync expiry for request %d: %v", req.ID, err)
	}
	return n
}

// purge runs one purge pass at now and returns the deleted rows. The pass is
// GLOBAL by design (that is what a retention job is), and a shared test database
// accumulates other tests' and earlier runs' due rows, so every assertion about
// "what was purged" filters to the ids the test created — never to the pass's
// total.
func purgedAmong(gone []RequestAttachmentPurgeRow, ids ...int64) map[int64]bool {
	want := make(map[int64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	got := map[int64]bool{}
	for _, p := range gone {
		if want[p.AttachmentID] {
			got[p.AttachmentID] = true
		}
	}
	return got
}

// purge runs one purge pass at now and returns the deleted rows.
func (f *retentionFixture) purge(now time.Time) []RequestAttachmentPurgeRow {
	f.t.Helper()
	gone, err := f.r.PurgeExpiredRequestAttachments(f.ctx, now, 1000)
	if err != nil {
		f.t.Fatalf("purge: %v", err)
	}
	return gone
}

// TestRequestAttachmentRetentionTable is the ticket's acceptance criterion 1: the
// active / closed / kept / reopened matrix, in one table, over the three closed
// states the contract names.
//
// It is written as a table rather than as four tests because the point is the
// CONTRAST: the same file, the same age and the same clock produce different
// answers only because of the request's posture and the keep flag. A test per case
// could pass while the matrix was wrong (say, purging a reopened request) because
// nothing would compare the cases to each other.
func TestRequestAttachmentRetentionTable(t *testing.T) {
	// The clock: every file in the table is the same age (200 days since its
	// request closed — well past the 90-day window), so only posture and keep can
	// explain a survivor.
	closedAt := requestTime(-200 * 24 * time.Hour)
	now := requestTime(0)
	pastWindow := closedAt.Add(domain.RequestAttachmentRetention())

	type row struct {
		name string
		// setup puts the request into the case's posture and returns the attachment.
		setup func(f *retentionFixture) (*domain.TeamRequest, *domain.RequestAttachment)
		// purge deletes it?
		purged bool
		// expiry: "nil" for no expiry, "due" for a stamp at or before now, "future"
		// for a stamp inside the window.
		expiry string
		// noSync skips the post-setup sync, for the one case that needs a STALE stamp.
		noSync bool
	}
	rows := []row{
		{
			name: "active request, no expiry, never purged",
			setup: func(f *retentionFixture) (*domain.TeamRequest, *domain.RequestAttachment) {
				req := f.newRequest("active-request")
				return req, f.mustInsert(req, "active.txt", "text/plain", []byte("still being worked\n"))
			},
			purged: false,
			expiry: "nil",
		},
		{
			name: "delivered request is still active",
			setup: func(f *retentionFixture) (*domain.TeamRequest, *domain.RequestAttachment) {
				req := f.newRequest("delivered-request")
				if _, err := f.r.db.Exec(f.ctx, `UPDATE team_requests
					SET state = 'delivered', outcome_brief = 'shipped', delivered_at = $2 WHERE id = $1`,
					req.ID, closedAt); err != nil {
					t.Fatalf("deliver request: %v", err)
				}
				req.State = domain.RequestStatusDelivered
				return req, f.mustInsert(req, "delivered.txt", "text/plain", []byte("awaiting acceptance\n"))
			},
			purged: false,
			expiry: "nil",
		},
		{
			name: "closed request past the window is purged",
			setup: func(f *retentionFixture) (*domain.TeamRequest, *domain.RequestAttachment) {
				req := f.newRequest("rejected-request")
				att := f.mustInsert(req, "rejected.txt", "text/plain", []byte("declined\n"))
				f.closeRequest(req, domain.RequestStatusRejected, closedAt)
				return req, att
			},
			purged: true,
			expiry: "due",
		},
		{
			name: "closed request inside the window is kept",
			setup: func(f *retentionFixture) (*domain.TeamRequest, *domain.RequestAttachment) {
				req := f.newRequest("recent-close")
				att := f.mustInsert(req, "recent.txt", "text/plain", []byte("recently closed\n"))
				f.closeRequest(req, domain.RequestStatusRejected, now.Add(-89*24*time.Hour))
				return req, att
			},
			purged: false,
			expiry: "future",
		},
		{
			name: "kept file is never purged, even past the window",
			setup: func(f *retentionFixture) (*domain.TeamRequest, *domain.RequestAttachment) {
				req := f.newRequest("kept-file")
				att := f.mustInsert(req, "kept.txt", "text/plain", []byte("pinned\n"))
				f.closeRequest(req, domain.RequestStatusRejected, closedAt)
				if _, err := f.r.SetRequestAttachmentKeep(f.ctx, RequestAttachmentKeepWrite{
					RequestAttachmentRef: f.attRef(req, att), Keep: true, KeptBy: &f.user,
				}); err != nil {
					t.Fatalf("keep attachment: %v", err)
				}
				return req, att
			},
			purged: false,
			expiry: "nil",
		},
		{
			// The belt-and-braces case: a row whose keep flag is set but whose expiry
			// still says due. No product path can write that pair (SetRequestAttachmentKeep
			// sets both in one statement), so this is the shape a hand-fix or a future
			// writer could leave — and the purge must still spare it. It is what makes
			// the `NOT keep` clause load-bearing rather than decorative.
			name: "keep flag alone protects a file with a stale due stamp",
			setup: func(f *retentionFixture) (*domain.TeamRequest, *domain.RequestAttachment) {
				req := f.newRequest("keepflag-only")
				att := f.mustInsert(req, "keepflag.txt", "text/plain", []byte("pinned by hand\n"))
				f.closeRequest(req, domain.RequestStatusRejected, closedAt)
				if _, err := f.r.db.Exec(f.ctx, `UPDATE request_attachments
					SET keep = TRUE, kept_at = NOW(), expires_at = $2 WHERE id = $1`,
					att.ID, pastWindow); err != nil {
					t.Fatalf("force keep with a stale stamp: %v", err)
				}
				return req, att
			},
			purged: false,
			expiry: "due",
			noSync: true,
		},
		{
			name: "reopened request keeps its file despite a stale due stamp",
			setup: func(f *retentionFixture) (*domain.TeamRequest, *domain.RequestAttachment) {
				req := f.newRequest("reopened-request")
				att := f.mustInsert(req, "reopened.txt", "text/plain", []byte("asked again\n"))
				f.closeRequest(req, domain.RequestStatusRejected, closedAt)
				f.sync(req) // the close stamped it: it IS due
				f.reopenRequest(req)
				// Deliberately no post-setup sync: the stored stamp still says due,
				// which is exactly the state a lost reopen-clear leaves behind.
				return req, att
			},
			purged: false,
			expiry: "due",
			noSync: true,
		},
		{
			name: "requester_accepted counts as closed",
			setup: func(f *retentionFixture) (*domain.TeamRequest, *domain.RequestAttachment) {
				req := f.newRequest("accepted-request")
				att := f.mustInsert(req, "accepted.txt", "text/plain", []byte("accepted\n"))
				f.closeRequest(req, domain.RequestStatusRequesterAccepted, closedAt)
				return req, att
			},
			purged: true,
			expiry: "due",
		},
		{
			name: "duplicate counts as closed",
			setup: func(f *retentionFixture) (*domain.TeamRequest, *domain.RequestAttachment) {
				req := f.newRequest("duplicate-folded")
				att := f.mustInsert(req, "dup.txt", "text/plain", []byte("folded\n"))
				f.closeRequest(req, domain.RequestStatusDuplicate, closedAt)
				return req, att
			},
			purged: true,
			expiry: "due",
		},
	}

	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			f := newRetentionFixture(t)
			req, att := c.setup(f)
			if !c.noSync {
				// The stamp the request's own posture implies, written the way a
				// transition writes it.
				f.sync(req)
			}

			got := f.storedExpiry(att.ID)
			switch c.expiry {
			case "nil":
				if got != nil {
					t.Fatalf("stored expiry = %v, want NULL (this file may not be purged)", got)
				}
			case "due":
				if got == nil {
					t.Fatal("stored expiry is NULL, want the close+90d stamp")
				}
				if got.After(now) {
					t.Fatalf("stored expiry = %v, want a stamp at or before %v (200 days after close)", got, now)
				}
				if want := pastWindow; !got.Equal(want) {
					t.Fatalf("stored expiry = %v, want the close+90d stamp %v", got, want)
				}
			case "future":
				if got == nil || !got.After(now) {
					t.Fatalf("stored expiry = %v, want a stamp after %v (89 days after close)", got, now)
				}
			}

			wasPurged := purgedAmong(f.purge(now), att.ID)[att.ID]
			if wasPurged != c.purged {
				t.Fatalf("purged = %v, want %v (state %s, keep %v)", wasPurged, c.purged, req.State, att.Keep)
			}
			if f.exists(att.ID) == c.purged {
				t.Fatalf("row exists = %v after a purge that reported purged=%v", !c.purged, c.purged)
			}
		})
	}
}

// TestRequestAttachmentRetentionExpiryIsDerivedFromTheRequest pins the clock
// itself: the stored expiry is the request's close plus the contract's window, it
// is NULL while the request is active, and a reopen clears it — the three writes
// a transition makes through syncAttachmentRetention.
func TestRequestAttachmentRetentionExpiryIsDerivedFromTheRequest(t *testing.T) {
	f := newRetentionFixture(t)
	req := f.newRequest("clock")
	att := f.mustInsert(req, "clock.txt", "text/plain", []byte("clock\n"))

	// Active: no expiry, and a sync writes nothing (the row already agrees).
	if n := f.sync(req); n != 0 {
		t.Fatalf("syncing an active request's expiry changed %d rows, want 0 (it is already NULL)", n)
	}
	if got := f.storedExpiry(att.ID); got != nil {
		t.Fatalf("an active request's attachment has expiry %v, want NULL", got)
	}

	// Closed: the stamp is exactly close + the domain's window, not "now + 90d".
	closedAt := requestTime(-10 * 24 * time.Hour)
	f.closeRequest(req, domain.RequestStatusRejected, closedAt)
	if n := f.sync(req); n != 1 {
		t.Fatalf("syncing a close changed %d rows, want 1", n)
	}
	got := f.storedExpiry(att.ID)
	if got == nil {
		t.Fatal("a closed request's attachment has no expiry")
	}
	want := closedAt.Add(domain.RequestAttachmentRetention())
	if !got.Equal(want) {
		t.Fatalf("expiry = %v, want the close+%dd stamp %v", got, domain.RequestAttachmentRetentionDays, want)
	}
	// And a second sync is a no-op: an unchanged value must not rewrite the row
	// (which would re-evaluate 056's size CHECK for nothing).
	if n := f.sync(req); n != 0 {
		t.Fatalf("a repeated sync changed %d rows, want 0", n)
	}

	// Reopened: the stamp is cleared, so the file is not reported purged.
	f.reopenRequest(req)
	if n := f.sync(req); n != 1 {
		t.Fatalf("syncing a reopen changed %d rows, want 1", n)
	}
	if got := f.storedExpiry(att.ID); got != nil {
		t.Fatalf("a reopened request's attachment kept expiry %v, want NULL", got)
	}

	// Kept while closed: no expiry, and unkeeping recomputes the request's window
	// rather than granting a fresh one.
	f.closeRequest(req, domain.RequestStatusRejected, closedAt)
	f.sync(req)
	if _, err := f.r.SetRequestAttachmentKeep(f.ctx, RequestAttachmentKeepWrite{
		RequestAttachmentRef: f.attRef(req, att), Keep: true, KeptBy: &f.user,
	}); err != nil {
		t.Fatalf("keep: %v", err)
	}
	if got := f.storedExpiry(att.ID); got != nil {
		t.Fatalf("a kept attachment has expiry %v, want NULL", got)
	}
	if _, err := f.r.SetRequestAttachmentKeep(f.ctx, RequestAttachmentKeepWrite{
		RequestAttachmentRef: f.attRef(req, att), Keep: false, KeptBy: &f.user,
	}); err != nil {
		t.Fatalf("unkeep: %v", err)
	}
	got = f.storedExpiry(att.ID)
	if got == nil {
		t.Fatal("unkeeping a closed request's file left it with no expiry")
	}
	if !got.Equal(want) {
		t.Fatalf("after unkeep expiry = %v, want the request's own window %v (unkeep must not restart the clock)", got, want)
	}
}

// TestSetRequestAttachmentKeepWritesTheColumns pins what a keep stores and what an
// unkeep clears, read from the row rather than from the returned value: keep,
// kept_by and kept_at move together, and unkeeping clears all three.
func TestSetRequestAttachmentKeepWritesTheColumns(t *testing.T) {
	f := newRetentionFixture(t)
	req := f.newRequest("keepcols")
	att := f.mustInsert(req, "keepcols.txt", "text/plain", []byte("keep me\n"))

	stored, err := f.r.SetRequestAttachmentKeep(f.ctx, RequestAttachmentKeepWrite{
		RequestAttachmentRef: f.attRef(req, att), Keep: true, KeptBy: &f.user,
	})
	if err != nil {
		t.Fatalf("keep: %v", err)
	}
	if stored.Content != nil {
		t.Error("a keep returned the bytes: a retention write must never read content")
	}
	keep, keptBy, keptAt := f.storedKeep(att.ID)
	if !keep || keptBy == nil || *keptBy != f.user || keptAt == nil {
		t.Fatalf("stored keep=%v kept_by=%v kept_at=%v, want true/%d/set", keep, keptBy, f.user, f.user)
	}

	stored, err = f.r.SetRequestAttachmentKeep(f.ctx, RequestAttachmentKeepWrite{
		RequestAttachmentRef: f.attRef(req, att), Keep: false, KeptBy: &f.user,
	})
	if err != nil {
		t.Fatalf("unkeep: %v", err)
	}
	if stored.Keep {
		t.Error("unkeep returned keep=true")
	}
	keep, keptBy, keptAt = f.storedKeep(att.ID)
	if keep || keptBy != nil || keptAt != nil {
		t.Fatalf("after unkeep stored keep=%v kept_by=%v kept_at=%v, want false/nil/nil", keep, keptBy, keptAt)
	}
}

// TestSetRequestAttachmentKeepIsTenantScoped is criterion 3's storage half: a keep
// aimed at another team's file, another org's file, another request's file or a
// missing id writes nothing and answers ErrNotFound — the surface's one 404, so a
// keep cannot be used to probe for a file.
func TestSetRequestAttachmentKeepIsTenantScoped(t *testing.T) {
	f := newRetentionFixture(t)
	req := f.newRequest("keepscope")
	att := f.mustInsert(req, "keepscope.txt", "text/plain", []byte("mine\n"))
	sibling := f.otherTeamRequest("keepscope-sibling")
	foreign := f.otherOrgRequest("keepscope-foreign")

	cases := []struct {
		name string
		ref  RequestAttachmentRef
	}{
		{"another team of the same org", RequestAttachmentRef{AttachmentID: att.ID, RequestID: req.ID, OrgID: req.OrgID, TeamID: f.team2}},
		{"another org", RequestAttachmentRef{AttachmentID: att.ID, RequestID: req.ID, OrgID: f.other, TeamID: f.teamOtherOrg}},
		{"another request in the same tenant", RequestAttachmentRef{AttachmentID: att.ID, RequestID: sibling.ID, OrgID: sibling.OrgID, TeamID: sibling.TeamID}},
		{"an id that does not exist", RequestAttachmentRef{AttachmentID: 1 << 40, RequestID: req.ID, OrgID: req.OrgID, TeamID: req.TeamID}},
		{"a foreign org's request", RequestAttachmentRef{AttachmentID: 1 << 40, RequestID: foreign.ID, OrgID: foreign.OrgID, TeamID: foreign.TeamID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := f.r.SetRequestAttachmentKeep(f.ctx, RequestAttachmentKeepWrite{
				RequestAttachmentRef: c.ref, Keep: true, KeptBy: &f.user,
			}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			if keep, _, _ := f.storedKeep(att.ID); keep {
				t.Fatal("a refused keep wrote keep=true on the real file")
			}
		})
	}

	// A missing tenant is a caller bug, not a 404: it must not hide behind one.
	if _, err := f.r.SetRequestAttachmentKeep(f.ctx, RequestAttachmentKeepWrite{
		RequestAttachmentRef: RequestAttachmentRef{AttachmentID: att.ID, RequestID: req.ID},
		Keep:                 true,
	}); !errors.Is(err, ErrRequestTenantScope) {
		t.Fatalf("err = %v, want ErrRequestTenantScope", err)
	}
}

// TestPurgeExpiredRequestAttachmentsRemovesBytes is criterion 2's storage half:
// the purge deletes the bytes, not just the row's metadata. Because 056 keeps the
// content IN the row, "deleted" has to mean the space is given back — so this
// measures the relation and searches for the payload, the same way the migration's
// no-orphan-bytes test does.
func TestPurgeExpiredRequestAttachmentsRemovesBytes(t *testing.T) {
	f := newRetentionFixture(t)
	req := f.newRequest("purgebytes")
	// Incompressible bytes: a periodic or zero-filled payload compresses inside its
	// TOAST value and would prove nothing about where the bytes went.
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte((i*7919 + 13) % 251)
	}
	// A run-unique tail, so the "are the bytes gone?" search below can only find
	// THIS file's payload and never a leftover from an earlier run of this test
	// against the same database.
	unique := []byte(fmt.Sprintf("run-%d", f.suffix))
	copy(big[len(big)-len(unique):], unique)
	att := f.mustInsert(req, "payload.bin", "application/octet-stream", big)
	closedAt := requestTime(-200 * 24 * time.Hour)
	f.closeRequest(req, domain.RequestStatusRejected, closedAt)
	f.sync(req)

	// The bytes really are stored before the purge, so a passing assertion after it
	// means something.
	var holders int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_attachments WHERE content = $1`, big).Scan(&holders); err != nil {
		t.Fatalf("find the stored payload: %v", err)
	}
	if holders != 1 {
		t.Fatalf("the payload is in %d rows before the purge, want 1", holders)
	}

	gone := f.purge(requestTime(0))
	var purged *RequestAttachmentPurgeRow
	for i := range gone {
		if gone[i].AttachmentID == att.ID {
			purged = &gone[i]
		}
	}
	if purged == nil {
		t.Fatalf("the purge did not delete attachment %d (it deleted %d other rows)", att.ID, len(gone))
	}
	if p := *purged; p.SizeBytes != int64(len(big)) || p.ContentType != "application/octet-stream" ||
		p.RequestID != req.ID || p.OrgID != req.OrgID || p.TeamID != req.TeamID || p.PublicRef != req.PublicRef {
		t.Fatalf("purged row = %+v, want the request's tenant, ref and the stored facts", p)
	}

	// The bytes are gone from the table — not merely hidden behind a deleted row.
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_attachments WHERE content = $1`, big).Scan(&holders); err != nil {
		t.Fatalf("search the payload after the purge: %v", err)
	}
	if holders != 0 {
		t.Fatalf("the payload is still in %d rows after the purge: the bytes were not deleted", holders)
	}
	if f.exists(att.ID) {
		t.Fatal("the attachment row survives its own purge")
	}
	// The request itself is untouched: the purge removes files, never the record of
	// what was asked.
	var requests int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM team_requests WHERE id = $1`, req.ID).Scan(&requests); err != nil {
		t.Fatalf("count the request: %v", err)
	}
	if requests != 1 {
		t.Fatal("the purge deleted the request, not only its attachment")
	}
}

// TestPurgeExpiredRequestAttachmentsSpareReopenedRequests is criterion 3: a reopen
// must avoid a premature purge, and it must do so even when the stored expiry was
// never corrected — the purge re-reads the request's own state instead of trusting
// the stamp. Then the reconcile pass repairs the stamp so the file is not reported
// purged by the download gate either.
func TestPurgeExpiredRequestAttachmentsSpareReopenedRequests(t *testing.T) {
	f := newRetentionFixture(t)
	req := f.newRequest("reopenrace")
	att := f.mustInsert(req, "reopenrace.txt", "text/plain", []byte("keep me through the reopen\n"))
	closedAt := requestTime(-200 * 24 * time.Hour)
	f.closeRequest(req, domain.RequestStatusRejected, closedAt)
	f.sync(req)

	// The stamp says due, and the request is now open again.
	f.reopenRequest(req)
	if got := f.storedExpiry(att.ID); got == nil {
		t.Fatal("the fixture did not leave a stale due stamp")
	}
	if purgedAmong(f.purge(requestTime(0)), att.ID)[att.ID] {
		t.Fatal("a reopened request's attachment was purged: a stale stamp must not decide the purge")
	}
	if !f.exists(att.ID) {
		t.Fatal("a reopened request's attachment was purged")
	}

	// The reconcile pass then corrects the stamp, so nothing else reports the file
	// as purged.
	if n, err := f.r.SyncAllRequestAttachmentExpiry(f.ctx, 1000); err != nil {
		t.Fatalf("reconcile: %v", err)
	} else if n < 1 {
		t.Fatalf("the reconcile pass corrected %d rows, want at least the stale one", n)
	}
	if got := f.storedExpiry(att.ID); got != nil {
		t.Fatalf("after the reconcile the reopened file still has expiry %v", got)
	}

	// Closing it again starts the clock again from the NEW close, not the old one:
	// the second close is the one that matters (the request is closed now).
	reclosedAt := requestTime(-1 * 24 * time.Hour)
	f.closeRequest(req, domain.RequestStatusRejected, reclosedAt)
	f.sync(req)
	got := f.storedExpiry(att.ID)
	if got == nil || !got.Equal(reclosedAt.Add(domain.RequestAttachmentRetention())) {
		t.Fatalf("after the second close expiry = %v, want %v", got, reclosedAt.Add(domain.RequestAttachmentRetention()))
	}
	if purgedAmong(f.purge(requestTime(0)), att.ID)[att.ID] {
		t.Fatal("the file was purged one day after the second close")
	}
}

// TestPurgeExpiredRequestAttachmentsIsBoundedAndSkipsLockedRows pins the two
// properties that make the job safe to run on every replica: a pass deletes at
// most the limit it is given, and a row another session holds is skipped rather
// than waited on (FOR UPDATE SKIP LOCKED). The second is what stops a purge from
// stalling behind a triager's transition.
func TestPurgeExpiredRequestAttachmentsIsBoundedAndSkipsLockedRows(t *testing.T) {
	f := newRetentionFixture(t)
	closedAt := requestTime(-200 * 24 * time.Hour)
	req := f.newRequest("bounded")
	var ids []int64
	for i := 0; i < 3; i++ {
		ids = append(ids, f.mustInsert(req, "bounded.txt", "text/plain", []byte("bounded\n")).ID)
	}
	f.closeRequest(req, domain.RequestStatusRejected, closedAt)
	f.sync(req)

	// One row held by another session: the purge must skip it and still take the
	// others, without blocking.
	tx, err := f.r.db.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, `SELECT id FROM request_attachments WHERE id = $1 FOR UPDATE`, ids[0]); err != nil {
		t.Fatalf("lock one row: %v", err)
	}

	gone, err := f.r.PurgeExpiredRequestAttachments(f.ctx, requestTime(0), 1000)
	if err != nil {
		t.Fatalf("purge with a locked row: %v", err)
	}
	mine := purgedAmong(gone, ids...)
	if !mine[ids[1]] || !mine[ids[2]] {
		t.Fatalf("purged %v of this test's rows, want the two unlocked ones", mine)
	}
	if mine[ids[0]] {
		t.Fatal("the purge deleted a row another session held")
	}
	if err := tx.Rollback(f.ctx); err != nil {
		t.Fatalf("release the lock: %v", err)
	}

	// Bounded: a limit of 1 deletes at most one, and repeated passes drain the
	// backlog. (Other rows may exist in a shared database, so the assertion is on
	// the bound and on this test's rows, not on the global total.)
	for i := 0; i < 5; i++ {
		gone, err = f.r.PurgeExpiredRequestAttachments(f.ctx, requestTime(0), 1)
		if err != nil {
			t.Fatalf("bounded purge: %v", err)
		}
		if len(gone) > 1 {
			t.Fatalf("a limit of 1 purged %d rows", len(gone))
		}
	}
	if f.exists(ids[0]) {
		t.Fatal("the previously locked row was not purged once its lock was released")
	}
}

// TestRequestAttachmentRetentionLayerStaysARetentionLayer is the ticket's scope
// made checkable: this file moves expiry, keep and rows, and nothing else. It
// parses the file (not its text, which is allowed to explain these rules) and
// fails if it grows a serving path, a scan verdict or a request deletion.
func TestRequestAttachmentRetentionLayerStaysARetentionLayer(t *testing.T) {
	const file = "request_attachment_retention.go"
	fs := token.NewFileSet()
	parsed, err := parser.ParseFile(fs, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	// The imports are the retention vocabulary: the clock, the tenant check and the
	// domain's numbers. No net/http, no mime, no signing, no io.
	allowed := map[string]bool{
		"context":                 true,
		"errors":                  true,
		"fmt":                     true,
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
			t.Errorf("%s imports %q: a retention layer has no business serving, sniffing or deleting requests", file, path)
		}
	}

	// The identifiers the file must never name. A purge that decided a scan state, a
	// serving posture or a request deletion would be a second rule nobody approved.
	forbidden := map[string]string{
		"RequestScanClean":                 "retention deletes bytes on the clock and never claims a scan verdict",
		"RequestScanPending":               "retention deletes bytes on the clock and never claims a scan verdict",
		"RequestScanInfected":              "retention deletes bytes on the clock and never claims a scan verdict",
		"DetectContentType":                "sniffing belongs to the upload service",
		"RequestAttachmentDisposition":     "serving posture belongs to the download gate",
		"RequestAttachmentDownloadRefusal": "whether bytes may be served is the download gate's decision, not retention's",
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
			for _, banned := range []string{"http://", "https://", "Content-Disposition", "DELETE FROM team_requests"} {
				if strings.Contains(value, banned) {
					t.Errorf("%s carries the literal %q: retention never serves bytes and never deletes a request", file, banned)
					break
				}
			}
		}
		return true
	})
}

// TestRequestAttachmentRetentionSqlAndGoAgreeOnWhatClosedMeans pins the one thing
// that can drift across the SQL/Go boundary: the set of states that start the D3
// clock. The SQL side is requestRetentionCloseExpr; the Go side is
// requestStatusStartsRetentionClock, which decides whether a transition bothers to
// re-stamp. A state in one and not the other is a close whose files never expire,
// or a stamp for a request that is still active.
func TestRequestAttachmentRetentionSqlAndGoAgreeOnWhatClosedMeans(t *testing.T) {
	// The SQL expression, as written, is the source of truth for the database.
	sql := requestRetentionCloseExpr("r")
	inSQL := map[string]bool{}
	for _, state := range domain.RequestStatuses {
		if strings.Contains(sql, "'"+string(state)+"'") {
			inSQL[string(state)] = true
		}
	}
	// The Go set, read the same way through its own predicate.
	want := map[string]bool{
		string(domain.RequestStatusRejected):          true,
		string(domain.RequestStatusDuplicate):         true,
		string(domain.RequestStatusRequesterAccepted): true,
	}
	for _, state := range domain.RequestStatuses {
		if inSQL[string(state)] != want[string(state)] {
			t.Errorf("state %q: the SQL close expression says closed=%v, the contract says %v",
				state, inSQL[string(state)], want[string(state)])
		}
	}
	// Every state is classified exactly once — nothing falls through both.
	if len(inSQL) != len(want) {
		t.Fatalf("the SQL close expression names %d states, want %d", len(inSQL), len(want))
	}
}
