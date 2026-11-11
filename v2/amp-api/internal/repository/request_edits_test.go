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
// refused without writing, and the edit's history row lands in the same
// transaction and names the fields without echoing the text.
//
// The record shape itself is migration 062's: a current-rev edit appends one
// request_events row of action `edit` (empty from/to, ids-and-field-names detail)
// and writes no request_comments row at all, while a failing event insert rolls
// the content and the revision back with it —
// TestUpdateRequestRecordsTheEditInHistory and
// TestUpdateRequestFailedEventInsertRollsBack.

import (
	"context"
	"encoding/json"
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

// editEvents returns every `edit` history row on a request, in append order —
// the history an edit writes. It reads request_events, not request_comments,
// because that IS the fix: an edit is an act, and acts live in the team's
// history rail (migration 062's verb).
func (f *requestRowsFixture) editEvents(requestID int) []domain.RequestEvent {
	f.t.Helper()
	return f.events(requestID, string(domain.RequestActionEdit))
}

// events returns a request's history rows of one action, in append order.
func (f *requestRowsFixture) events(requestID int, action string) []domain.RequestEvent {
	f.t.Helper()
	rows, err := f.r.db.Query(f.ctx, `SELECT id, request_id, org_id, team_id, action, from_state, to_state,
		actor, actor_user_id, detail, created_at
		FROM request_events WHERE request_id = $1 AND action = $2 ORDER BY id`, requestID, action)
	if err != nil {
		f.t.Fatalf("read %s events for %d: %v", action, requestID, err)
	}
	defer rows.Close()
	out := []domain.RequestEvent{}
	for rows.Next() {
		var ev domain.RequestEvent
		if err := rows.Scan(&ev.ID, &ev.RequestID, &ev.OrgID, &ev.TeamID, &ev.Action,
			&ev.FromStatus, &ev.ToStatus, &ev.Actor, &ev.ActorUserID, &ev.Detail, &ev.CreatedAt); err != nil {
			f.t.Fatalf("scan %s event: %v", action, err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("read %s events for %d: %v", action, requestID, err)
	}
	return out
}

// eventCount is how many history rows of ANY action a request has, so a test can
// say "exactly one row was appended" rather than "one row of this verb".
func (f *requestRowsFixture) eventCount(requestID int) int {
	f.t.Helper()
	var n int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_events WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		f.t.Fatalf("count history for %d: %v", requestID, err)
	}
	return n
}

// commentCount is how many discussion rows (any kind) a request has. An edit must
// write NONE of them: the record moved to the history rail, and duplicating it
// into the requester-visible thread is exactly what the approved shape forbids.
func (f *requestRowsFixture) commentCount(requestID int) int {
	f.t.Helper()
	var n int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_comments WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		f.t.Fatalf("count comments for %d: %v", requestID, err)
	}
	return n
}

// editDetail decodes the single edit event of one edit; it fails the test when the
// count is not exactly one, so callers can assert on the detail without repeating
// the count check.
func (f *requestRowsFixture) editDetail(requestID int) (domain.RequestEvent, editEventDetail) {
	f.t.Helper()
	events := f.editEvents(requestID)
	if len(events) != 1 {
		f.t.Fatalf("edit events = %d (%v), want exactly one", len(events), events)
	}
	var detail editEventDetail
	if err := json.Unmarshal([]byte(events[0].Detail), &detail); err != nil {
		f.t.Fatalf("decode edit detail %q: %v", events[0].Detail, err)
	}
	return events[0], detail
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

	// The conflict is the F2 slice's 409-with-current-revision: the shared
	// RequestRevConflict carries the rev the row is actually at, so a client can
	// retry with the right If-Match without a second round trip.
	var revErr *RequestRevConflict
	if !errors.As(err, &revErr) {
		t.Fatalf("stale edit error = %T %v, want *RequestRevConflict", err, err)
	}
	if revErr.Expected != 1 || revErr.Current != 2 {
		t.Errorf("conflict = {expected %d, current %d}, want {expected 1, current 2}",
			revErr.Expected, revErr.Current)
	}
	if revErr.Current != current.Rev {
		t.Errorf("conflict current rev = %d, want the winner's %d", revErr.Current, current.Rev)
	}
	// The message tells the caller to reload rather than reporting a bare number.
	if !strings.Contains(revErr.Error(), "reload") {
		t.Errorf("conflict message %q does not tell the caller what to do", revErr.Error())
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

	// A failed edit writes no history: the history rail shows only the winner's.
	if events := f.editEvents(req.ID); len(events) != 1 {
		t.Errorf("edit events = %d (%v), want exactly the one successful edit", len(events), events)
	}
}

// TestUpdateRequestConcurrentEditorsOneWinner is acceptance criterion 2's race
// half: N editors all holding the same revision submit distinct subjects at the
// same instant; exactly one may win, the rest must conflict, the revision must
// have moved exactly once, and the stored text must be a whole value from the
// winner (never interleaved or partially applied).
//
// It also pins that every loser is told the CURRENT revision (rev 2), never the
// stale one it read: a client retrying on the number it already had would loop.
// Note what this test alone does NOT prove: with goroutines starting together the
// winner often commits before the losers read, so they conflict from an honest
// post-commit read and the lock never has to block anything. The lock itself is
// proved deterministically by TestUpdateRequestBlocksOnAHeldRowLock.
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
	// Every loser was told the truth about where the row actually is. (The winner's
	// text is checked against the re-read below; the conflict carries the revision,
	// not a copy of the row, so that one error shape serves a claim race and a
	// stale edit alike.)
	for i, conflict := range losers {
		if conflict.Current != req.Rev+1 {
			t.Errorf("loser %d's conflict says the row is at rev %d, want %d (the winner's): a loser reporting the stale revision it already had would make a client loop",
				i, conflict.Current, req.Rev+1)
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
	// One write, one event: a loser's transaction rolled back whole, including its
	// history row.
	if events := f.editEvents(req.ID); len(events) != 1 {
		t.Errorf("edit events = %d (%v), want exactly one for one winner", len(events), events)
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

	// None of the probes wrote anything, and none wrote a history row.
	reread, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, f.team)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if reread.Rev != req.Rev || reread.Subject != req.Subject {
		t.Errorf("a refused edit changed the row: rev %d subject %q", reread.Rev, reread.Subject)
	}
	if events := f.editEvents(req.ID); len(events) != 0 {
		t.Errorf("refused edits left history rows: %v", events)
	}
	if n := f.commentCount(req.ID); n != 0 {
		t.Errorf("refused edits left %d discussion rows, want 0", n)
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

	if events := f.editEvents(req.ID); len(events) != 0 {
		t.Errorf("no-op edits left history rows: %v", events)
	}
	if n := f.commentCount(req.ID); n != 0 {
		t.Errorf("no-op edits left %d discussion rows, want 0", n)
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

// TestUpdateRequestRecordsTheEditInHistory is the acceptance criterion's record
// shape (approved #3873, migration 062): a current-rev edit writes EXACTLY ONE
// request_events row of action `edit`, with empty from/to states and a detail of
// field names and the revision transition only — and NO request_comments row at
// all. The comment the pre-062 code wrote is gone; an edit is an act, and acts
// live in the team's history rail.
func TestUpdateRequestRecordsTheEditInHistory(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)

	secret := "a brief that must not be copied into the history: SECRET-TOKEN-42"
	subject := "SSO for the portal"
	got := f.mustEdit(req, RequestEdit{Subject: &subject, InputBrief: &secret})
	if got.Rev != req.Rev+1 {
		t.Fatalf("rev after the edit = %d, want %d", got.Rev, req.Rev+1)
	}

	// Exactly one history row, of action `edit`. The fixture request was inserted
	// directly (no submit row), so "one row" is a real count of what the edit did.
	all := f.events(req.ID, string(domain.RequestActionEdit))
	if len(all) != 1 {
		t.Fatalf("edit events = %d (%v), want exactly one", len(all), all)
	}
	if n := f.eventCount(req.ID); n != 1 {
		t.Errorf("request %d has %d history rows after one edit, want 1", req.ID, n)
	}
	ev, detail := f.editDetail(req.ID)

	// A non-transition verb names no states (054's request_events_transition_states
	// rule): an edit can never fake a lifecycle move.
	if ev.FromStatus != "" || ev.ToStatus != "" {
		t.Errorf("edit event carries states %q -> %q, want both empty", ev.FromStatus, ev.ToStatus)
	}
	// Tenant columns are copied from the request, not asked of the caller.
	if ev.OrgID != req.OrgID || ev.TeamID != req.TeamID {
		t.Errorf("edit event tenant = org %d team %d, want org %d team %d", ev.OrgID, ev.TeamID, req.OrgID, req.TeamID)
	}
	// The detail names the changed fields and the revision transition, in a stable
	// order, and never the text.
	if strings.Join(detail.Fields, ",") != "subject,input_brief" {
		t.Errorf("edit event fields = %v, want [subject input_brief]", detail.Fields)
	}
	if detail.FromRev != 1 || detail.ToRev != 2 {
		t.Errorf("edit event revs = %d -> %d, want 1 -> 2", detail.FromRev, detail.ToRev)
	}
	if strings.Contains(ev.Detail, "SECRET-TOKEN-42") || strings.Contains(ev.Detail, subject) {
		t.Errorf("the history row echoes request text: %q", ev.Detail)
	}

	// The edit wrote no comment: the requester-visible thread stays empty, because
	// duplicating an act into it is exactly what the approved shape drops.
	if n := f.commentCount(req.ID); n != 0 {
		t.Errorf("an edit wrote %d request_comments rows, want 0", n)
	}

	// The row is attributed to the editor, with the account linked.
	if ev.Actor != "desk@edits.test" {
		t.Errorf("edit event actor = %q, want the editor's display name", ev.Actor)
	}
	if ev.ActorUserID == nil || *ev.ActorUserID != f.user {
		t.Errorf("edit event actor_user_id = %v, want %d", ev.ActorUserID, f.user)
	}

	// A connector-style editor with no AMP user is still attributable by name, and
	// a second edit appends a second row (append-only: never an update in place).
	second := "SSO for the portal, corrected"
	if _, _, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, got.Rev,
		RequestEdit{Subject: &second}, RequestEditActor{Name: "github-connector"}); err != nil {
		t.Fatalf("connector edit: %v", err)
	}
	events := f.editEvents(req.ID)
	if len(events) != 2 {
		t.Fatalf("edit events = %d (%v), want two", len(events), events)
	}
	if events[1].ActorUserID != nil {
		t.Errorf("connector edit actor_user_id = %v, want nil (no AMP user)", *events[1].ActorUserID)
	}
	if events[1].Actor != "github-connector" {
		t.Errorf("connector edit actor = %q, want its integration identity", events[1].Actor)
	}
	if n := f.commentCount(req.ID); n != 0 {
		t.Errorf("two edits wrote %d request_comments rows, want 0", n)
	}
}

// TestUpdateRequestFailedEventInsertRollsBack is the acceptance criterion's
// atomicity half: the content write, the rev bump and the edit event are one
// transaction, so a failing event insert leaves NOTHING behind — the revision does
// not move, the subject does not change, and no history row exists.
//
// The failure is injected by a trigger on request_events scoped to this test's
// request id, so the edit's own INSERT really is the statement that fails (rather
// than the repository's error path being simulated).
func TestUpdateRequestFailedEventInsertRollsBack(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)

	fn := fmt.Sprintf("request_edits_test_fail_event_%d", f.suffix)
	if _, err := f.r.db.Exec(f.ctx, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %[1]s() RETURNS trigger AS $$
		BEGIN
			IF NEW.action = 'edit' AND NEW.request_id = %[2]d THEN
				RAISE EXCEPTION 'injected failure while recording the edit event';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql`, fn, req.ID)); err != nil {
		t.Fatalf("create the failure injector: %v", err)
	}
	dropInjector := func() {
		t.Helper()
		if _, err := f.r.db.Exec(f.ctx, `DROP TRIGGER IF EXISTS request_edits_test_fail_event ON request_events`); err != nil {
			t.Fatalf("drop the failure injector trigger: %v", err)
		}
		if _, err := f.r.db.Exec(f.ctx, `DROP FUNCTION IF EXISTS `+fn+`()`); err != nil {
			t.Fatalf("drop the failure injector function: %v", err)
		}
	}
	t.Cleanup(dropInjector)
	if _, err := f.r.db.Exec(f.ctx, `CREATE TRIGGER request_edits_test_fail_event
		BEFORE INSERT ON request_events FOR EACH ROW EXECUTE FUNCTION `+fn+`()`); err != nil {
		t.Fatalf("install the failure injector: %v", err)
	}

	subject := "a subject the failed transaction must not store"
	got, changed, err := f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, req.Rev,
		RequestEdit{Subject: &subject}, f.editActor())
	if err == nil {
		t.Fatalf("the injected event failure produced no error (row %v, changed %v)", got, changed)
	}
	if got != nil || changed {
		t.Fatalf("a rolled-back edit returned row %v changed=%v", got, changed)
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
		t.Errorf("an event failure read as %v; it is a server fault, not a client answer", err)
	}

	// Nothing moved: same rev, same subject, and no history row at all.
	reread, getErr := f.r.GetRequest(f.ctx, req.ID, req.OrgID, f.team)
	if getErr != nil {
		t.Fatalf("re-read: %v", getErr)
	}
	if reread.Rev != req.Rev || reread.Subject != req.Subject {
		t.Errorf("the rolled-back edit wrote: rev %d subject %q (want rev %d %q)",
			reread.Rev, reread.Subject, req.Rev, req.Subject)
	}
	if n := f.eventCount(req.ID); n != 0 {
		t.Errorf("%d history rows survived the rolled-back edit, want 0", n)
	}

	// With the fault gone the same edit at the same revision succeeds, which is what
	// proves the rollback left no stuck state behind.
	dropInjector()
	got, changed, err = f.r.UpdateRequest(f.ctx, req.ID, req.OrgID, f.team, req.Rev,
		RequestEdit{Subject: &subject}, f.editActor())
	if err != nil || !changed {
		t.Fatalf("retry after the fault: err %v changed %v", err, changed)
	}
	if got.Rev != req.Rev+1 {
		t.Errorf("retry rev = %d, want %d", got.Rev, req.Rev+1)
	}
	if events := f.editEvents(req.ID); len(events) != 1 {
		t.Errorf("retry edit events = %d (%v), want exactly one", len(events), events)
	}
}

// TestUpdateRequestNoteFollowsTheRequestLifecycle proves the history row the edit
// writes is not orphaned: it hangs off the request, so deleting the request (or,
// via 054's composite cascades, its team) takes the row with it. An orphan row
// under a reused id would be a cross-tenant read.
func TestUpdateRequestNoteFollowsTheRequestLifecycle(t *testing.T) {
	f := newRequestRowsFixture(t)
	req := f.newEditableRequest(nil)
	subject := "an edit whose history row must die with its request"

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
	if len(f.editEvents(req.ID)) != 1 {
		t.Error("the edit landed without its history event")
	}

	if _, err := f.r.db.Exec(f.ctx, `DELETE FROM team_requests WHERE id = $1`, req.ID); err != nil {
		t.Fatalf("delete request: %v", err)
	}
	var remaining int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_events WHERE request_id = $1`, req.ID).
		Scan(&remaining); err != nil {
		t.Fatalf("count history after delete: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d history rows survived their request's deletion", remaining)
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
			t.Fatalf("error %T does not carry the current revision", err)
		}
		if conflict.Current != req.Rev+1 {
			t.Errorf("conflict current rev = %d, want the holder's %d (the committed revision, not the one the blocked edit read)",
				conflict.Current, req.Rev+1)
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
	if events := f.editEvents(req.ID); len(events) != 0 {
		t.Errorf("the conflicted edit left a history row: %v", events)
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
