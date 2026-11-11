package repository

// Tests for request_edits.go against a real Postgres (AMP #3873, epic 202 story
// 535). They run only when TEST_DATABASE_URL is set, like the row-contract tests
// next door:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository/... -run 'UpdateRequest' -count=1 -race
//
// The three promises this layer makes are exactly the ticket's acceptance
// criteria, and each has a test that would fail if the promise were dropped:
//
//  1. an edit at the current revision succeeds and increments rev exactly once —
//     TestUpdateRequestCurrentRevEditsAndBumpsOnce;
//  2. a stale (concurrent) edit conflicts instead of overwriting — both the
//     sequential stale case and two writers racing at the same revision, of which
//     exactly one may win — TestUpdateRequestStaleRevConflicts and
//     TestUpdateRequestConcurrentEditorsOneWinner;
//  3. identity, snapshot, source and state cannot be overwritten through the
//     payload — structurally (RequestEdit has no such field) and behaviourally
//     (nothing about them moves after an edit) —
//     TestUpdateRequestCannotReachIdentityOrState;
//  4. the row contract is reused unchanged, so an edit answers the same shape a
//     read does — TestUpdateRequestReturnsTheStoredRow.
//
// The remaining cases are the ones that would otherwise be discovered three
// slices from now: another tenant's request is a 404 and not a conflict, a
// missing tenant is a caller bug, a cancelled context is neither, a no-op edit
// does not churn the revision, an edit that would break a whole-row invariant is
// refused without writing, and the history note lands in the same transaction
// and names the fields without echoing the text.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// newEditableRequest writes one request this fixture owns and returns it as
// stored (read back through GetRequest, so the test's baseline is the same shape
// an editor would have read).
//
// The public_ref is unique per call — a counter, not a field count — because
// public_ref is unique across the whole table and several subtests create a
// request with the same field set.
var editableRequestSeq atomic.Int64

func (f *requestRowsFixture) newEditableRequest(fields map[string]any) *domain.TeamRequest {
	f.t.Helper()
	n := editableRequestSeq.Add(1)
	req := &domain.TeamRequest{
		PublicRef:        fmt.Sprintf("editable%06d%09d", n, f.suffix),
		OrgID:            f.org,
		TeamID:           f.team,
		RequesterUserID:  &f.user,
		RequesterContact: "requester@edits.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "Add SSO to the customer portal",
		InputBrief:       "We need SAML for our enterprise customers.",
		State:            domain.RequestStatusNew,
		Rev:              1,
		Metadata:         domain.Metadata{"integration": "github"},
		CreatedAt:        requestTime(-1 * time.Hour),
		UpdatedAt:        requestTime(-1 * time.Hour),
	}
	for key, value := range fields {
		switch key {
		case "subject":
			req.Subject = value.(string)
		case "input_brief":
			req.InputBrief = value.(string)
		case "outcome_brief":
			req.OutcomeBrief = value.(string)
		case "state":
			req.State = value.(domain.RequestStatus)
		case "delivered_at":
			at := value.(time.Time)
			req.DeliveredAt = &at
		case "owner":
			owner := value.(int)
			req.OwnerUserID = &owner
		case "claim":
			owner := value.(int)
			req.ClaimUserID = &owner
			claimed := requestTime(-30 * time.Minute)
			req.ClaimedAt = &claimed
		case "source_external_id":
			req.SourceKind = domain.RequestSourceConnector
			req.SourceExternalID = value.(string)
		case "metadata":
			req.Metadata = value.(domain.Metadata)
		default:
			f.t.Fatalf("newEditableRequest: unknown field %q", key)
		}
	}
	req.ID = f.insertRequest(req)
	got, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		f.t.Fatalf("read back request %d: %v", req.ID, err)
	}
	return got
}

// editActor is a plain attributable editor for the tests.
func (f *requestRowsFixture) editActor() RequestEditActor {
	return RequestEditActor{Name: "desk@edits.test", UserID: &f.user}
}

// mustEdit applies one edit and fails the test if it errors.
func (f *requestRowsFixture) mustEdit(req *domain.TeamRequest, edit RequestEdit) *domain.TeamRequest {
	f.t.Helper()
	got, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, req.TeamID, req.Rev, edit, f.editActor())
	if err != nil {
		f.t.Fatalf("UpdateRequest(%d at rev %d): %v", req.ID, req.Rev, err)
	}
	if !changed {
		f.t.Fatalf("UpdateRequest(%d) reported no change for an edit that should have changed something", req.ID)
	}
	return got
}

// editNotes returns every `system` comment body on a request, in append order —
// the history the edit writes.
func (f *requestRowsFixture) editNotes(requestID int) []string {
	f.t.Helper()
	rows, err := f.r.db.Query(f.ctx, `SELECT body FROM request_comments
		WHERE request_id = $1 AND kind = $2 ORDER BY id`, requestID, string(domain.RequestCommentSystem))
	if err != nil {
		f.t.Fatalf("read edit notes for %d: %v", requestID, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			f.t.Fatalf("scan edit note: %v", err)
		}
		out = append(out, body)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("read edit notes for %d: %v", requestID, err)
	}
	return out
}

// TestUpdateRequestCurrentRevEditsAndBumpsOnce is acceptance criterion 1: an edit
// at the current revision applies, and the revision moves exactly one step — not
// zero (a lost update) and not two (a double bump from the row and a trigger).
func TestUpdateRequestCurrentRevEditsAndBumpsOnce(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)
	if req.Rev != 1 {
		t.Fatalf("fixture rev = %d, want 1", req.Rev)
	}

	subject := "  Add SSO and SCIM to the customer portal  " // trimmed by normalisation
	brief := "We need SAML for SSO, and SCIM for user lifecycle.\n\nToday they sign in with a password."
	got := f.mustEdit(req, RequestEdit{Subject: &subject, InputBrief: &brief})

	if got.Rev != req.Rev+1 {
		t.Errorf("rev after edit = %d, want %d (exactly one step)", got.Rev, req.Rev+1)
	}
	if got.Subject != "Add SSO and SCIM to the customer portal" {
		t.Errorf("subject = %q, want the trimmed value", got.Subject)
	}
	if got.InputBrief != brief {
		t.Errorf("input_brief = %q, want %q", got.InputBrief, brief)
	}
	if got.OutcomeBrief != req.OutcomeBrief {
		t.Errorf("outcome_brief moved to %q although the edit did not name it", got.OutcomeBrief)
	}

	// The stored row — not just the returned value — is what moved.
	reread, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if reread.Rev != req.Rev+1 || reread.Subject != got.Subject || reread.InputBrief != got.InputBrief {
		t.Errorf("stored row = rev %d subject %q; want rev %d subject %q",
			reread.Rev, reread.Subject, req.Rev+1, got.Subject)
	}
	if !reread.UpdatedAt.After(req.UpdatedAt) {
		t.Errorf("updated_at = %v, want it after the pre-edit value %v", reread.UpdatedAt, req.UpdatedAt)
	}

	// The next edit builds on the new revision, so the CAS chain continues.
	next := "Add SSO, SCIM and audit-log export"
	again := f.mustEdit(got, RequestEdit{Subject: &next})
	if again.Rev != req.Rev+2 {
		t.Errorf("second edit rev = %d, want %d", again.Rev, req.Rev+2)
	}
	if again.InputBrief != brief {
		t.Errorf("second edit changed input_brief to %q although it did not name it", again.InputBrief)
	}
}

// TestUpdateRequestStaleRevConflicts is acceptance criterion 2, the sequential
// half: an edit carrying a revision the row has moved past is refused, wins
// nothing, and reports the revision the row is actually at.
func TestUpdateRequestStaleRevConflicts(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)
	first := "First writer's subject"
	current := f.mustEdit(req, RequestEdit{Subject: &first})

	// A second editor that read rev 1 (before the first write) now submits.
	stale := "Second writer's subject"
	conflicting, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, req.TeamID, req.Rev,
		RequestEdit{Subject: &stale}, f.editActor())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale edit error = %v, want ErrConflict", err)
	}
	if changed {
		t.Error("stale edit reported changed=true")
	}
	if conflicting != nil {
		t.Errorf("stale edit returned a row (%v); want nil so the caller cannot mistake it for success", conflicting)
	}

	// The conflict is the F2 slice's 409-with-current-record: it carries the row.
	var revErr *RequestRevConflict
	if !errors.As(err, &revErr) {
		t.Fatalf("stale edit error = %T %v, want *RequestRevConflict", err, err)
	}
	if revErr.Expected != 1 || revErr.Current == nil || revErr.Current.Rev != 2 {
		t.Errorf("conflict = {expected %d, current %v}, want {expected 1, current rev 2}",
			revErr.Expected, revErr.Current)
	}
	if revErr.Current.Subject != first {
		t.Errorf("conflict's current record subject = %q, want the winner's %q", revErr.Current.Subject, first)
	}
	// The conflict's row is a snapshot of what is stored, not the rejected payload.
	if strings.Contains(revErr.Current.Subject, stale) {
		t.Error("conflict's current record carries the rejected payload")
	}
	if revErr.Current.Rev != current.Rev {
		t.Errorf("conflict current rev = %d, want the winner's %d", revErr.Current.Rev, current.Rev)
	}

	// Nothing the loser sent landed, and the winner's value is intact on disk.
	reread, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if reread.Subject != first || reread.Rev != 2 {
		t.Errorf("stored row after the conflict = rev %d subject %q; want the winner's rev 2 %q",
			reread.Rev, reread.Subject, first)
	}

	// A failed edit writes no history: the note trail shows only the winner.
	if notes := f.editNotes(req.ID); len(notes) != 1 {
		t.Errorf("edit notes = %d (%v), want exactly the one successful edit", len(notes), notes)
	}
}

// TestUpdateRequestConcurrentEditorsOneWinner is acceptance criterion 2's race
// half: N editors all holding the same revision submit distinct subjects at the
// same instant; exactly one may win, the rest must conflict, the revision must
// have moved exactly once, and the stored text must be a whole value from the
// winner (never interleaved or partially applied).
//
// It also pins what the row lock buys, which the WHERE predicate alone does not:
// each loser's conflict must carry the CURRENT row (rev 2, the winner's text),
// not the rev-1 row the loser read before it blocked. Without FOR UPDATE every
// loser would report "the row is at rev 1" — the revision it already knew — and a
// client retrying on that number would loop forever. That assertion is what makes
// this test fail when the lock is removed.
func TestUpdateRequestConcurrentEditorsOneWinner(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)

	const editors = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		losers  []*RequestRevConflict
		start   = make(chan struct{})
	)
	for i := 0; i < editors; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			subject := fmt.Sprintf("editor %d's subject", i)
			<-start
			_, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, req.Rev,
				RequestEdit{Subject: &subject}, f.editActor())
			mu.Lock()
			defer mu.Unlock()
			var conflict *RequestRevConflict
			switch {
			case err == nil && changed:
				winners = append(winners, subject)
			case errors.Is(err, ErrConflict) && errors.As(err, &conflict):
				losers = append(losers, conflict)
			default:
				t.Errorf("editor %d: unexpected error %v (changed=%v)", i, err, changed)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("%d editors won (%v); exactly one may", len(winners), winners)
	}
	if len(losers) != editors-1 {
		t.Fatalf("%d conflicts, want %d", len(losers), editors-1)
	}
	// Every loser was told the truth about where the row actually is.
	for i, conflict := range losers {
		if conflict.Current == nil {
			t.Errorf("loser %d's conflict carries no current row", i)
			continue
		}
		if conflict.Current.Rev != req.Rev+1 {
			t.Errorf("loser %d's conflict says the row is at rev %d, want %d (the winner's): without the lock the loser reports the stale revision it already had",
				i, conflict.Current.Rev, req.Rev+1)
		}
		if conflict.Current.Subject != winners[0] {
			t.Errorf("loser %d's conflict carries subject %q, want the winner's %q",
				i, conflict.Current.Subject, winners[0])
		}
	}

	reread, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, f.team)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if reread.Rev != req.Rev+1 {
		t.Errorf("rev after %d racing editors = %d, want %d (only the winner bumped it)",
			editors, reread.Rev, req.Rev+1)
	}
	if reread.Subject != winners[0] {
		t.Errorf("stored subject = %q, want the winner's %q", reread.Subject, winners[0])
	}
	// One write, one note: a loser's transaction rolled back whole, including its
	// history row.
	if notes := f.editNotes(req.ID); len(notes) != 1 {
		t.Errorf("edit notes = %d (%v), want exactly one for one winner", len(notes), notes)
	}
}

// TestUpdateRequestCannotReachIdentityOrState is acceptance criterion 3. It has
// two halves: the type has no way to express the change (compile-time, asserted
// by reflection so a future field cannot be added quietly), and an edit that
// changes content leaves every immutable field byte-identical.
func TestUpdateRequestCannotReachIdentityOrState(t *testing.T) {
	// (a) The payload's field set is the editable surface, no more. A new field on
	// RequestEdit must be a deliberate decision about editability, not a drive-by.
	allowed := map[string]bool{
		"Subject": true, "InputBrief": true, "OutcomeBrief": true, "Metadata": true,
	}
	typ := reflect.TypeOf(RequestEdit{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !allowed[name] {
			t.Errorf("RequestEdit grew a field %q; it is only editable if the UPDATE and this test say so", name)
		}
	}

	f := newRequestRowsFixture(t)
	// A row with every immutable field populated, so "nothing moved" is a real
	// comparison rather than nil == nil.
	req := f.newEditableRequest(map[string]any{
		"state":              domain.RequestStatusDelivered,
		"outcome_brief":      "SSO ships for SAML providers; SCIM is not included.",
		"delivered_at":       requestTime(-10 * time.Minute),
		"owner":              f.other2,
		"claim":              f.user,
		"source_external_id": "gh-issue-9101",
		"metadata":           domain.Metadata{"integration": "github", "issue": 9101, "vip": true},
	})

	subject := "Corrected subject line"
	outcome := "SSO ships for SAML providers; SCIM is included from 2026-12."
	got := f.mustEdit(req, RequestEdit{
		Subject:      &subject,
		OutcomeBrief: &outcome,
		Metadata:     domain.MetadataPatch{"triage": "escalated", "vip": nil},
	})

	// Content moved (and the label patch merged per key: triage added, vip removed,
	// integration and issue kept because the patch did not name them).
	if got.Subject != subject || got.OutcomeBrief != outcome {
		t.Fatalf("content did not move: subject %q outcome %q", got.Subject, got.OutcomeBrief)
	}
	if _, stillThere := got.Metadata["vip"]; stillThere {
		t.Error("metadata patch did not remove the key it named with null")
	}
	if got.Metadata["triage"] != "escalated" {
		t.Errorf("metadata[triage] = %v, want escalated", got.Metadata["triage"])
	}
	if got.Metadata["integration"] != "github" || got.Metadata["issue"] != float64(9101) {
		t.Errorf("metadata patch dropped keys it did not name: %v", got.Metadata)
	}

	// Everything the ticket calls immutable is exactly as it was, field by field,
	// with the timestamps compared as instants.
	if got.ID != req.ID || got.PublicRef != req.PublicRef || got.OrgID != req.OrgID || got.TeamID != req.TeamID {
		t.Errorf("identity moved: id %d pubref %q org %d team %d", got.ID, got.PublicRef, got.OrgID, got.TeamID)
	}
	if !sameIntPtr(got.RequesterUserID, req.RequesterUserID) || got.RequesterContact != req.RequesterContact {
		t.Errorf("requester snapshot moved: user %v contact %q", got.RequesterUserID, got.RequesterContact)
	}
	if got.SourceKind != req.SourceKind || got.SourceExternalID != req.SourceExternalID {
		t.Errorf("source identity moved: kind %q external id %q", got.SourceKind, got.SourceExternalID)
	}
	if got.State != req.State {
		t.Errorf("state moved from %q to %q through a content edit", req.State, got.State)
	}
	if !sameIntPtr(got.ClaimUserID, req.ClaimUserID) || !sameTimePtr(got.ClaimedAt, req.ClaimedAt) {
		t.Errorf("triage claim moved: user %v at %v", got.ClaimUserID, got.ClaimedAt)
	}
	if !sameIntPtr(got.OwnerUserID, req.OwnerUserID) {
		t.Errorf("assignee moved to %v", got.OwnerUserID)
	}
	if !sameTimePtr(got.DeliveredAt, req.DeliveredAt) {
		t.Errorf("delivered_at moved to %v", got.DeliveredAt)
	}
	if got.ReopenedCount != req.ReopenedCount || !sameTimePtr(got.ClosedAt, req.ClosedAt) ||
		!sameTimePtr(got.ReopenedAt, req.ReopenedAt) || !sameIntPtr(got.DuplicateOf, req.DuplicateOf) {
		t.Error("a lifecycle field other than the content moved through a content edit")
	}
	if !got.CreatedAt.Equal(req.CreatedAt) {
		t.Errorf("created_at moved from %v to %v", req.CreatedAt, got.CreatedAt)
	}
}

// TestUpdateRequestReturnsTheStoredRow pins that the returned value is what the
// database holds, produced by the shared scanner — not a locally patched guess.
// request_rows.go owns that contract and this slice must not fork it.
func TestUpdateRequestReturnsTheStoredRow(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)
	subject := "A subject that is stored verbatim"
	got := f.mustEdit(req, RequestEdit{Subject: &subject})

	reread, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	assertRequestEqual(t, fmt.Sprintf("updated request %d vs a fresh read", req.ID), got, reread)
}

// TestUpdateRequestTenantScopeIsNotFound pins that a wrong tenant is the same
// 404 an invisible row gets — never a conflict — so an id walker learns nothing,
// while a missing tenant and a nonsense revision stay caller bugs.
func TestUpdateRequestTenantScopeIsNotFound(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)
	subject := "tenant probes never land"
	edit := RequestEdit{Subject: &subject}

	cases := []struct {
		name             string
		requestID        int
		org, team        int
		wantNotFound     bool
		wantTenantScope  bool
		wantConflictFree bool
	}{
		{name: "sibling team in the same org", requestID: req.ID, org: f.org, team: f.team2, wantNotFound: true},
		{name: "same team number in another org", requestID: req.ID, org: f.other, team: f.team, wantNotFound: true},
		{name: "unknown request id", requestID: req.ID + 9_000_000, org: f.org, team: f.team, wantNotFound: true},
		{name: "no team", requestID: req.ID, org: f.org, team: 0, wantTenantScope: true},
		{name: "no org", requestID: req.ID, org: 0, team: f.team, wantTenantScope: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, changed, err := f.r.UpdateRequest(f.ctx, tc.requestID, tc.org, tc.team, req.Rev, edit, f.editActor())
			if changed {
				t.Error("a refused edit reported changed=true")
			}
			switch {
			case tc.wantNotFound:
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("error = %v, want ErrNotFound", err)
				}
				if errors.Is(err, ErrRequestTenantScope) {
					t.Error("a wrong tenant was reported as a missing tenant")
				}
			case tc.wantTenantScope:
				if !errors.Is(err, ErrRequestTenantScope) {
					t.Fatalf("error = %v, want ErrRequestTenantScope", err)
				}
				if errors.Is(err, ErrNotFound) {
					t.Error("a missing tenant was reported as a missing request")
				}
			}
			// Neither shape may ever read as a resolvable conflict.
			if errors.Is(err, ErrConflict) {
				t.Error("a tenant refusal read as ErrConflict; a caller would offer a retry that cannot win")
			}
		})
	}

	// None of the probes wrote anything, and none wrote a history note.
	reread, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, f.team)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if reread.Rev != req.Rev || reread.Subject != req.Subject {
		t.Errorf("a refused edit changed the row: rev %d subject %q", reread.Rev, reread.Subject)
	}
	if notes := f.editNotes(req.ID); len(notes) != 0 {
		t.Errorf("refused edits left history notes: %v", notes)
	}
}

// TestUpdateRequestLowRevisionIsACallerBug: rev starts at 1, so an expected
// revision below it cannot name a row. Reporting it as a conflict would invite a
// retry that can never win.
func TestUpdateRequestLowRevisionIsACallerBug(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)
	subject := "x"
	for _, expected := range []int{0, -3} {
		_, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, expected,
			RequestEdit{Subject: &subject}, f.editActor())
		if changed || !errors.Is(err, ErrRequestTenantScope) || errors.Is(err, ErrConflict) {
			t.Errorf("expected rev %d: err = %v changed=%v, want ErrRequestTenantScope and not a conflict",
				expected, err, changed)
		}
	}
}

// TestUpdateRequestCancelledContextIsNotNotFound: a real load failure must never
// read as a missing request or a conflict, or a handler reports a database
// problem as "no such request" and the caller retries a conflict that never
// existed.
func TestUpdateRequestCancelledContextIsNotNotFound(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)
	subject := "never written"

	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	_, _, err := f.r.UpdateRequest(ctx, req.ID, req.OrgID, f.team, req.Rev,
		RequestEdit{Subject: &subject}, f.editActor())
	if err == nil {
		t.Fatal("a cancelled context edited the request")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("cancelled context read as ErrNotFound: %v", err)
	}
	if errors.Is(err, ErrConflict) {
		t.Errorf("cancelled context read as a revision conflict: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
}

// TestUpdateRequestNoOpChangesNothing: an edit that names fields but moves no
// value (or names nothing at all) must not bump the revision — otherwise every
// repeated form save invalidates every other holder's revision for no reason.
func TestUpdateRequestNoOpChangesNothing(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)

	t.Run("empty edit", func(t *testing.T) {
		got, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, req.Rev, RequestEdit{}, f.editActor())
		if err != nil || changed {
			t.Fatalf("empty edit: err %v changed %v, want no error and no change", err, changed)
		}
		if got == nil || got.Rev != req.Rev {
			t.Errorf("empty edit returned %v, want the stored row at rev %d", got, req.Rev)
		}
	})

	t.Run("same values", func(t *testing.T) {
		same := req.Subject
		brief := req.InputBrief
		got, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, req.Rev,
			RequestEdit{Subject: &same, InputBrief: &brief, Metadata: domain.MetadataPatch{"integration": "github"}},
			f.editActor())
		if err != nil || changed {
			t.Fatalf("same-value edit: err %v changed %v, want no error and no change", err, changed)
		}
		if got.Rev != req.Rev {
			t.Errorf("same-value edit moved rev to %d", got.Rev)
		}
	})

	// An empty metadata patch alone is also a no-op (nothing named, nothing to do).
	if _, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, req.Rev,
		RequestEdit{Metadata: domain.MetadataPatch{}}, f.editActor()); err != nil || changed {
		t.Errorf("empty metadata patch: err %v changed %v, want a no-op", err, changed)
	}

	// Removing a key that is already absent is a no-op too, not a change.
	if _, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, req.Rev,
		RequestEdit{Metadata: domain.MetadataPatch{"never-set": nil}}, f.editActor()); err != nil || changed {
		t.Errorf("removing an absent key: err %v changed %v, want a no-op", err, changed)
	}

	if notes := f.editNotes(req.ID); len(notes) != 0 {
		t.Errorf("no-op edits left history notes: %v", notes)
	}
}

// TestUpdateRequestRefusesInvalidAndInvariantBreakingEdits: the payload is
// validated through the domain before anything is written, so an edit cannot
// store what create would refuse, and cannot break a whole-row invariant that
// 053's CHECKs would otherwise reject with a constraint message.
func TestUpdateRequestRefusesInvalidAndInvariantBreakingEdits(t *testing.T) {
	f := newRequestRowsFixture(t)

	t.Run("blank subject", func(t *testing.T) {
		req := f.newEditableRequest(nil)
		blank := "   "
		_, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, req.Rev,
			RequestEdit{Subject: &blank}, f.editActor())
		if err == nil || changed {
			t.Fatalf("blank subject: err %v changed %v, want a refusal", err, changed)
		}
		if !strings.Contains(err.Error(), "subject") {
			t.Errorf("error %q does not name the field", err)
		}
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			t.Errorf("a validation refusal read as %v", err)
		}
		// A refusal before the write leaves rev and content untouched.
		reread, getErr := f.r.GetRequest(f.ctx, req.ID, req.OrgID, f.team)
		if getErr != nil {
			t.Fatalf("re-read: %v", getErr)
		}
		if reread.Rev != req.Rev || reread.Subject != req.Subject {
			t.Errorf("a refused edit wrote: rev %d subject %q", reread.Rev, reread.Subject)
		}
	})

	t.Run("brief over the bound", func(t *testing.T) {
		req := f.newEditableRequest(nil)
		huge := strings.Repeat("x", domain.MaxRequestBriefLen+1)
		_, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, req.Rev,
			RequestEdit{InputBrief: &huge}, f.editActor())
		if err == nil || changed {
			t.Fatalf("over-long brief: err %v changed %v, want a refusal", err, changed)
		}
		if !strings.Contains(err.Error(), "input_brief") {
			t.Errorf("error %q does not name the field", err)
		}
	})

	t.Run("bad metadata key", func(t *testing.T) {
		req := f.newEditableRequest(nil)
		// domain.MetadataPatch.Split refuses a key with leading/trailing space (it
		// would be a label nobody can match) and an over-long key. Both must be
		// refused here before anything is written.
		for _, bad := range []string{" padded", "padded ", strings.Repeat("k", domain.MaxMetadataKeyLen+1)} {
			_, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, req.Rev,
				RequestEdit{Metadata: domain.MetadataPatch{bad: "v"}}, f.editActor())
			if err == nil || changed {
				t.Errorf("metadata key %q: err %v changed %v, want a refusal", bad, err, changed)
			}
		}
	})

	t.Run("clearing the outcome brief of a delivered request", func(t *testing.T) {
		// 053 requires a delivered request to carry an outcome brief. An edit must
		// refuse that with the domain's sentence, not let the CHECK fail the write.
		req := f.newEditableRequest(map[string]any{
			"state":         domain.RequestStatusDelivered,
			"outcome_brief": "SSO ships for SAML providers.",
			"delivered_at":  requestTime(-5 * time.Minute),
		})
		empty := ""
		_, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, req.Rev,
			RequestEdit{OutcomeBrief: &empty}, f.editActor())
		if err == nil || changed {
			t.Fatalf("clearing a delivered request's outcome brief: err %v changed %v, want a refusal", err, changed)
		}
		// The refusal must be the DOMAIN's sentence, not 053's CHECK firing inside the
		// UPDATE: whole-row Validate is what turns a constraint into an answer a
		// service can return. A raw SQLSTATE means the guard was skipped and the
		// database caught it late, so the caller would get "violates check constraint"
		// instead of "delivered is evidenced".
		msg := err.Error()
		if strings.Contains(msg, "SQLSTATE") || strings.Contains(msg, "check constraint") || strings.Contains(msg, "23514") {
			t.Errorf("the refusal came from the database's CHECK, not the domain: %v", err)
		}
		if !strings.Contains(msg, "outcome_brief") && !strings.Contains(msg, "delivered") {
			t.Errorf("the refusal does not explain itself in domain terms: %v", err)
		}
		reread, getErr := f.r.GetRequest(f.ctx, req.ID, req.OrgID, f.team)
		if getErr != nil {
			t.Fatalf("re-read: %v", getErr)
		}
		if reread.OutcomeBrief != req.OutcomeBrief || reread.Rev != req.Rev {
			t.Errorf("the refused edit wrote: rev %d outcome %q", reread.Rev, reread.OutcomeBrief)
		}
	})
}

// TestUpdateRequestRecordsTheEditInHistory: the edit and its history row land in
// one transaction (so a reader never sees one without the other), the note names
// the changed fields and the revision transition, and it never echoes the text.
func TestUpdateRequestRecordsTheEditInHistory(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)

	secret := "a brief that must not be copied into the thread: SECRET-TOKEN-42"
	subject := "SSO for the portal"
	got := f.mustEdit(req, RequestEdit{Subject: &subject, InputBrief: &secret})

	notes := f.editNotes(req.ID)
	if len(notes) != 1 {
		t.Fatalf("edit notes = %d (%v), want exactly one", len(notes), notes)
	}
	note := notes[0]
	for _, want := range []string{"subject", "input_brief", "rev 1 → 2"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q does not mention %q", note, want)
		}
	}
	if strings.Contains(note, "SECRET-TOKEN-42") || strings.Contains(note, subject) {
		t.Errorf("the history note echoes request text: %q", note)
	}

	// The note is attributed to the editor, with the account linked.
	var author string
	var authorID *int
	err := f.r.db.QueryRow(f.ctx, `SELECT author, author_user_id FROM request_comments
		WHERE request_id = $1 AND kind = $2 ORDER BY id DESC LIMIT 1`, req.ID, string(domain.RequestCommentSystem)).
		Scan(&author, &authorID)
	if err != nil {
		t.Fatalf("read the note's attribution: %v", err)
	}
	if author != "desk@edits.test" {
		t.Errorf("note author = %q, want the editor's display name", author)
	}
	if authorID == nil || *authorID != f.user {
		t.Errorf("note author_user_id = %v, want %d", authorID, f.user)
	}

	// A connector-style editor with no AMP user is still attributable by name.
	second := "SSO for the portal, corrected"
	if _, _, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, got.Rev,
		RequestEdit{Subject: &second}, RequestEditActor{Name: "github-connector"}); err != nil {
		t.Fatalf("connector edit: %v", err)
	}
	notes = f.editNotes(req.ID)
	if len(notes) != 2 {
		t.Fatalf("edit notes = %d (%v), want two", len(notes), notes)
	}
	var nullAuthor *int
	if err := f.r.db.QueryRow(f.ctx, `SELECT author_user_id FROM request_comments
		WHERE request_id = $1 AND kind = $2 ORDER BY id DESC LIMIT 1`, req.ID, string(domain.RequestCommentSystem)).
		Scan(&nullAuthor); err != nil {
		t.Fatalf("read the connector note: %v", err)
	}
	if nullAuthor != nil {
		t.Errorf("connector note author_user_id = %v, want nil (no AMP user)", *nullAuthor)
	}
}

// TestUpdateRequestNoteFollowsTheRequestLifecycle proves the history row the edit
// writes is not orphaned: it hangs off the request, so deleting the request (or,
// via 054's composite cascades, its team) takes the note with it. An orphan note
// under a reused id would be a cross-tenant read.
func TestUpdateRequestNoteFollowsTheRequestLifecycle(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)
	subject := "an edit whose note must die with its request"

	got, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, req.Rev,
		RequestEdit{Subject: &subject}, f.editActor())
	if err != nil || !changed {
		t.Fatalf("edit: err %v changed %v", err, changed)
	}
	reread, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, f.team)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if reread.Rev != got.Rev || reread.Subject != subject {
		t.Error("stored row and returned row disagree after an edit")
	}
	if len(f.editNotes(req.ID)) != 1 {
		t.Error("the edit landed without its history note")
	}

	if _, err := f.r.db.Exec(f.ctx, `DELETE FROM team_requests WHERE id = $1`, req.ID); err != nil {
		t.Fatalf("delete request: %v", err)
	}
	var remaining int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_comments WHERE request_id = $1`, req.ID).
		Scan(&remaining); err != nil {
		t.Fatalf("count notes after delete: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d notes survived their request's deletion", remaining)
	}
	if _, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, f.team); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted request still reads back: %v", err)
	}
}

// TestUpdateRequestBlocksOnAHeldRowLock is the deterministic proof of the lock,
// which the racing-editor test above cannot give on its own: with 8 goroutines the
// winner often commits before the losers even read, so they conflict from an
// honest post-commit read and the FOR UPDATE never has to do any work.
//
// Here the row is held by an open transaction that has changed nothing yet. An
// edit started while that lock is held must BLOCK — it may not read rev 1 and may
// not answer — and when the holder commits its own change, the blocked edit must
// wake, see the new revision and conflict. That is the difference between a real
// compare-and-set and a read-then-check with a gap in the middle.
func TestUpdateRequestBlocksOnAHeldRowLock(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)

	holder, err := f.r.db.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(f.ctx)
	// Lock the row the way the edit's own SELECT does.
	held := &domain.TeamRequest{}
	if err := scanRequest(holder.QueryRow(f.ctx, requestSelect+requestTenantWhere+` FOR UPDATE`,
		req.ID, req.OrgID, req.TeamID), held); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	// The edit starts now and must not be able to finish while the lock is held.
	subject := "the editor's subject"
	done := make(chan error, 1)
	editCtx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	go func() {
		_, _, err := f.r.UpdateRequest(editCtx, req.ID, req.OrgID, f.team, req.Rev,
			RequestEdit{Subject: &subject}, f.editActor())
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("an edit completed while another transaction held the row lock (err=%v); the read is not locked", err)
	case <-time.After(400 * time.Millisecond):
		// Still blocked, which is the point. Not a flake-proof barrier, but the only
		// way an unlocked read can reach here is by being slower than 400 ms on a
		// primary-key lookup, and the branch below then proves it committed early.
	}

	// The holder now moves the row itself and commits.
	holderSubject := "the holder's subject"
	if _, err := holder.Exec(f.ctx,
		`UPDATE team_requests SET subject = $2, rev = rev + 1 WHERE id = $1`, req.ID, holderSubject); err != nil {
		t.Fatalf("holder update: %v", err)
	}
	if err := holder.Commit(f.ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}

	select {
	case err := <-done:
		// It woke up and must have conflicted, carrying the holder's new revision.
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("blocked edit error = %v, want ErrConflict against the holder's commit", err)
		}
		var conflict *RequestRevConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("error %T does not carry the current row", err)
		}
		if conflict.Current == nil || conflict.Current.Rev != req.Rev+1 {
			t.Errorf("conflict current = %v, want rev %d", conflict.Current, req.Rev+1)
		}
		if conflict.Current != nil && conflict.Current.Subject != holderSubject {
			t.Errorf("conflict current subject = %q, want the holder's %q",
				conflict.Current.Subject, holderSubject)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the blocked edit never returned after the lock was released")
	}

	// The holder's write won and the editor's did not.
	reread, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, f.team)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if reread.Subject != holderSubject || reread.Rev != req.Rev+1 {
		t.Errorf("stored = rev %d subject %q, want the holder's rev %d %q",
			reread.Rev, reread.Subject, req.Rev+1, holderSubject)
	}
	if notes := f.editNotes(req.ID); len(notes) != 0 {
		t.Errorf("the conflicted edit left a history note: %v", notes)
	}
}

// sameIntPtr and sameTimePtr compare optional columns by value (not address),
// with nil == nil, so an untouched nullable field cannot pass a comparison just
// because both sides are pointers.
func sameIntPtr(a, b *int) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func sameTimePtr(a, b *time.Time) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || a.Equal(*b)
}

// The row contract is a shared surface: this slice may not edit it. If someone
// adds a column to requestCols without teaching this file, the round-trip test
// (and every read) fails — this guard names the dependency so the failure is not
// mistaken for an edit bug.
var _ = func() any {
	// Compile-time proof the symbols this file relies on exist with the shapes it
	// expects: a scanner, a column list and the tenant predicate.
	var _ rowScanner = pgx.Row(nil)
	var _ = requestCols
	var _ = requestTenantWhere
	var _ queryRower = pgx.Tx(nil)
	return nil
}
