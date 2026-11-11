package repository

// Tests for request_claims.go against a real Postgres (AMP #3872, epic 202 story
// 535; build slice C2). They run only when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository/... -run 'RequestClaim|ReleaseClaim|SetAssignee' -count=1 -race
//
// The ticket's three criteria are what these assert, in order:
//
//  1. concurrent claimants have exactly one winner, and the win moves the row
//     exactly once: rev + 1, one `claim` history row, and the winner holds the
//     claim. Every loser is refused as RequestClaimHeld naming the winner and
//     writes nothing — including its own history row.
//  2. a release is idempotent: the second release (and a release of a request
//     that was never claimed) answers changed=false, leaves rev where it is and
//     appends no second event. A release by someone who does not hold the claim
//     is refused and also writes nothing.
//  3. an assignee transfer clears the claim in the same statement, records both
//     what it replaced and whose claim it cleared, and a replay of the same
//     transfer writes nothing at all. A wrong tenant writes nothing either — the
//     write is scoped by org AND team, not by id alone.
//
// Beyond the criteria, the tests pin the properties the code's comments claim:
// a claim never expires (a claim with an old claimed_at is still held), a claim is
// never stolen (a second claimant is refused, not silently given the row), a
// stale expected rev is a RequestRevConflict that carries the current rev, and a
// claim/assign naming a user who does not exist is an ordinary not-found rather
// than a constraint error escaping as a 5xx.
//
// Each test builds its own org, teams, users and requests with a random suffix
// (requestRowsFixture), so repeated runs and tests in the same database don't see
// each other's rows.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// requestClaimsFixture is requestRowsFixture plus the extra users a claim test
// needs (each claimant is its own person) and the history reader the assertions
// use.
type requestClaimsFixture struct {
	*requestRowsFixture
}

func newRequestClaimsFixture(t *testing.T) *requestClaimsFixture {
	t.Helper()
	return &requestClaimsFixture{requestRowsFixture: newRequestRowsFixture(t)}
}

// insertUser adds one user with a run-unique subject and email and returns its id.
func (f *requestClaimsFixture) insertUser(label string) int {
	f.t.Helper()
	var id int
	err := f.r.db.QueryRow(f.ctx, `INSERT INTO users (subject, email) VALUES ($1, $2) RETURNING id`,
		fmt.Sprintf("claims-%s-%d", label, f.suffix),
		fmt.Sprintf("claims-%s-%d@claims.test", label, f.suffix)).Scan(&id)
	if err != nil {
		f.t.Fatalf("insert user %s: %v", label, err)
	}
	return id
}

// newRequest writes one `new` request in the fixture's first team and returns it
// as stored (id, rev=1, no claim, no owner).
func (f *requestClaimsFixture) newRequest(label string) *domain.TeamRequest {
	f.t.Helper()
	base := requestTime(-time.Hour)
	req := &domain.TeamRequest{
		PublicRef:        f.ref("claims" + label),
		OrgID:            f.org,
		TeamID:           f.team,
		RequesterContact: "requester@claims.test",
		SourceKind:       domain.RequestSourceWeb,
		Subject:          "Triage me",
		State:            domain.RequestStatusNew,
		Rev:              1,
		Metadata:         domain.Metadata{},
		CreatedAt:        base,
		UpdatedAt:        base,
	}
	req.ID = f.insertRequest(req)
	return req
}

// events returns the request's history in append order, read straight from the
// table so the assertions see what was actually stored rather than what a method
// on the repository chose to return.
func (f *requestClaimsFixture) events(requestID int) []domain.RequestEvent {
	f.t.Helper()
	rows, err := f.r.db.Query(f.ctx, `SELECT id, request_id, org_id, team_id, action, from_state, to_state,
		actor, actor_user_id, detail, created_at
		FROM request_events WHERE request_id = $1 ORDER BY id`, requestID)
	if err != nil {
		f.t.Fatalf("history of request %d: %v", requestID, err)
	}
	defer rows.Close()
	var out []domain.RequestEvent
	for rows.Next() {
		var ev domain.RequestEvent
		if err := rows.Scan(&ev.ID, &ev.RequestID, &ev.OrgID, &ev.TeamID, &ev.Action,
			&ev.FromStatus, &ev.ToStatus, &ev.Actor, &ev.ActorUserID, &ev.Detail, &ev.CreatedAt); err != nil {
			f.t.Fatalf("scan history of request %d: %v", requestID, err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("history of request %d: %v", requestID, err)
	}
	return out
}

// actions summarises the request's history as action counts, which is how these
// tests state "one claim event and no others".
func (f *requestClaimsFixture) actions(requestID int) map[domain.RequestEventAction]int {
	f.t.Helper()
	counts := map[domain.RequestEventAction]int{}
	for _, ev := range f.events(requestID) {
		counts[ev.Action]++
	}
	return counts
}

// mustGet reloads the request through the tenant-scoped read, so assertions see
// the stored row rather than the value the write returned.
func (f *requestClaimsFixture) mustGet(req *domain.TeamRequest) *domain.TeamRequest {
	f.t.Helper()
	got, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		f.t.Fatalf("GetRequest(%d): %v", req.ID, err)
	}
	return got
}

// write is the caller's part of a claim write for this fixture's tenant and
// request, with the claimant and actor set. Tests vary the fields they care about.
func (f *requestClaimsFixture) write(req *domain.TeamRequest, claimant int) RequestClaimWrite {
	return RequestClaimWrite{
		RequestID:   req.ID,
		OrgID:       req.OrgID,
		TeamID:      req.TeamID,
		ClaimUserID: claimant,
		Actor:       fmt.Sprintf("triager-%d", claimant),
		ActorUserID: &claimant,
	}
}

// TestClaimRequestConcurrentOneWinner is criterion 1: two (here n) triagers
// claiming at once produce exactly one winner, and the row moves exactly once.
//
// The claims serialise on the request's row lock, so this is deterministic rather
// than scheduler luck: the first transaction to take the lock writes the claim and
// the rev bump, and every later one — including any that started before the winner
// committed — re-reads the row after the lock is released (READ COMMITTED, FOR
// UPDATE) and sees the claim it must not take.
func TestClaimRequestConcurrentOneWinner(t *testing.T) {
	f := newRequestClaimsFixture(t)
	req := f.newRequest("race00")

	const n = 8
	claimants := make([]int, n)
	for i := range claimants {
		claimants[i] = f.insertUser(fmt.Sprintf("racer%d", i))
	}

	isClaimant := map[int]bool{}
	for _, id := range claimants {
		isClaimant[id] = true
	}

	start := make(chan struct{})
	type result struct {
		i       int
		changed bool
		err     error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i, userID := range claimants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, changed, err := f.r.ClaimRequest(f.ctx, f.write(req, userID))
			results <- result{i: i, changed: changed, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var winners []result
	for res := range results {
		switch {
		case res.err == nil && res.changed:
			winners = append(winners, res)
		case res.err != nil:
			// A loser is refused, and refused as the loser of this race: the
			// holder it names must be another claimant, and no partial write may
			// have happened.
			if !errors.Is(res.err, ErrConflict) {
				t.Fatalf("claimant %d: err = %v, want a conflict (someone else won)", res.i, res.err)
			}
			var held *RequestClaimHeld
			if !errors.As(res.err, &held) {
				t.Fatalf("claimant %d: err = %v, want *RequestClaimHeld naming the holder", res.i, res.err)
			}
			if held.RequestID != req.ID {
				t.Errorf("claimant %d: refusal names request %d, want %d", res.i, held.RequestID, req.ID)
			}
			if !isClaimant[held.Holder] {
				t.Errorf("claimant %d: refusal names holder %d, who is not one of the claimants", res.i, held.Holder)
			}
		default:
			t.Fatalf("claimant %d: returned changed=false with no error, which only a replay may do", res.i)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("%d claimants won, want exactly 1", len(winners))
	}
	winner := claimants[winners[0].i]

	stored := f.mustGet(req)
	if stored.ClaimUserID == nil || *stored.ClaimUserID != winner {
		t.Fatalf("stored claim_user_id = %v, want the winner %d", stored.ClaimUserID, winner)
	}
	if stored.ClaimedAt == nil {
		t.Fatalf("the winner's claim has no claimed_at: 053 refuses a claim without one")
	}
	// One act, one write: the rev moved once no matter how many claimants raced.
	if stored.Rev != req.Rev+1 {
		t.Errorf("rev = %d after the race, want %d (exactly one write, not one per claimant)", stored.Rev, req.Rev+1)
	}
	counts := f.actions(req.ID)
	if counts[domain.RequestActionClaim] != 1 || len(counts) != 1 {
		t.Fatalf("history = %v, want exactly one claim event and nothing else", counts)
	}

	// The winner's own retry is a replay: no second event and no second rev bump.
	again, changed, err := f.r.ClaimRequest(f.ctx, f.write(req, winner))
	if err != nil {
		t.Fatalf("the winner claiming again: %v", err)
	}
	if changed {
		t.Fatalf("the winner's second claim reported a change, want a replay")
	}
	if again.Rev != stored.Rev {
		t.Errorf("the replay bumped rev to %d, want it left at %d", again.Rev, stored.Rev)
	}
	if !again.ClaimedAt.Equal(*stored.ClaimedAt) {
		t.Errorf("the replay moved claimed_at from %s to %s; the claim age must not reset",
			stored.ClaimedAt.UTC(), again.ClaimedAt.UTC())
	}
	if got := f.actions(req.ID)[domain.RequestActionClaim]; got != 1 {
		t.Errorf("the replay added a claim event: %d claim rows, want 1", got)
	}
}

// TestClaimRequestNeverExpiresOrSteals pins the two refusals that make a claim
// trustworthy: it does not lapse with time (D4), and it is not taken from its
// holder by a later claimant (a transfer is SetAssignee's explicit act).
func TestClaimRequestNeverExpiresOrSteals(t *testing.T) {
	f := newRequestClaimsFixture(t)
	req := f.newRequest("expiry0")
	holder, other := f.insertUser("holder"), f.insertUser("other")

	if _, changed, err := f.r.ClaimRequest(f.ctx, f.write(req, holder)); err != nil || !changed {
		t.Fatalf("first claim: changed=%v err=%v", changed, err)
	}
	// Backdate a month: a claim that expired on a clock would now be takeable.
	if _, err := f.r.db.Exec(f.ctx,
		`UPDATE team_requests SET claimed_at = NOW() - INTERVAL '30 days' WHERE id = $1`, req.ID); err != nil {
		t.Fatalf("backdate the claim: %v", err)
	}
	before := f.mustGet(req)

	if _, _, err := f.r.ClaimRequest(f.ctx, f.write(req, other)); !errors.Is(err, ErrConflict) {
		t.Fatalf("a month-old claim was taken from its holder: err=%v", err)
	}
	var held *RequestClaimHeld
	if _, _, err := f.r.ClaimRequest(f.ctx, f.write(req, other)); !errors.As(err, &held) || held.Holder != holder {
		t.Fatalf("refusal = %v, want *RequestClaimHeld naming the holder %d", err, holder)
	}
	after := f.mustGet(req)
	if after.Rev != before.Rev {
		t.Errorf("a refused claim bumped rev from %d to %d", before.Rev, after.Rev)
	}
	if after.ClaimUserID == nil || *after.ClaimUserID != holder {
		t.Errorf("claim_user_id = %v after a refused steal, want the holder %d", after.ClaimUserID, holder)
	}
	if got := f.actions(req.ID)[domain.RequestActionClaim]; got != 1 {
		t.Errorf("%d claim events after the refused steal, want 1", got)
	}
}

// TestReleaseClaimIsIdempotent is criterion 2: a release records one event and a
// replay records none — no duplicate history and no further rev bump — and a
// release by someone who is not the holder is refused without writing.
func TestReleaseClaimIsIdempotent(t *testing.T) {
	f := newRequestClaimsFixture(t)
	req := f.newRequest("release")
	holder, stranger := f.insertUser("holder"), f.insertUser("stranger")

	// Releasing a request nobody claimed is already released: no write, no event.
	unclaimed, changed, err := f.r.ReleaseClaim(f.ctx, f.write(req, holder))
	if err != nil {
		t.Fatalf("release of an unclaimed request: %v", err)
	}
	if changed {
		t.Fatalf("release of an unclaimed request reported a change")
	}
	if unclaimed.Rev != req.Rev {
		t.Errorf("release of an unclaimed request bumped rev to %d, want %d", unclaimed.Rev, req.Rev)
	}
	if len(f.events(req.ID)) != 0 {
		t.Fatalf("release of an unclaimed request wrote history: %v", f.actions(req.ID))
	}

	if _, _, err := f.r.ClaimRequest(f.ctx, f.write(req, holder)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	claimed := f.mustGet(req)

	// Someone else's release is refused: ending another person's claim is the
	// mirror of stealing it.
	if _, changed, err := f.r.ReleaseClaim(f.ctx, f.write(req, stranger)); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stranger's release: changed=%v err=%v, want a conflict", changed, err)
	}
	if held := f.mustGet(req); held.ClaimUserID == nil || *held.ClaimUserID != holder || held.Rev != claimed.Rev {
		t.Fatalf("a refused release changed the row: claim=%v rev=%d, want %d at rev %d",
			held.ClaimUserID, held.Rev, holder, claimed.Rev)
	}

	released, changed, err := f.r.ReleaseClaim(f.ctx, f.write(req, holder))
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !changed {
		t.Fatalf("the holder's release reported no change")
	}
	if released.ClaimUserID != nil || released.ClaimedAt != nil {
		t.Fatalf("after release claim = (%v, %v), want both nil", released.ClaimUserID, released.ClaimedAt)
	}
	if released.Rev != claimed.Rev+1 {
		t.Errorf("rev = %d after release, want %d", released.Rev, claimed.Rev+1)
	}

	// The replay: same answer as "already released", and no second anything.
	again, changed, err := f.r.ReleaseClaim(f.ctx, f.write(req, holder))
	if err != nil {
		t.Fatalf("release replay: %v", err)
	}
	if changed {
		t.Fatalf("the release replay reported a change")
	}
	if again.Rev != released.Rev {
		t.Errorf("the release replay bumped rev to %d, want %d", again.Rev, released.Rev)
	}
	counts := f.actions(req.ID)
	if counts[domain.RequestActionRelease] != 1 {
		t.Errorf("%d release events, want exactly 1 (the replay must not duplicate it)", counts[domain.RequestActionRelease])
	}
	if counts[domain.RequestActionClaim] != 1 || len(counts) != 2 {
		t.Errorf("history = %v, want exactly one claim and one release", counts)
	}

	// The event is attributable and explains itself.
	events := f.events(req.ID)
	var release *domain.RequestEvent
	for i, ev := range events {
		if ev.Action == domain.RequestActionRelease {
			release = &events[i]
		}
	}
	if release == nil {
		t.Fatalf("no release event stored")
	}
	if release.ActorUserID == nil || *release.ActorUserID != holder {
		t.Errorf("release event actor = %v, want user %d", release.ActorUserID, holder)
	}
	var detail releaseEventDetail
	if err := json.Unmarshal([]byte(release.Detail), &detail); err != nil {
		t.Fatalf("release detail %q: %v", release.Detail, err)
	}
	if detail.ReleasedClaimUserID != holder {
		t.Errorf("release detail names user %d, want the releasing holder %d", detail.ReleasedClaimUserID, holder)
	}
	if release.FromStatus != "" || release.ToStatus != "" {
		t.Errorf("a release event named states (%q->%q): it is not a transition (054's CHECK)", release.FromStatus, release.ToStatus)
	}
}

// TestSetAssigneeClearsClaim is criterion 3: a transfer sets the owner and clears
// the claim in one statement, records what it replaced, and a replay writes
// nothing. It also checks a transfer into a wrong tenant writes nothing.
func TestSetAssigneeClearsClaim(t *testing.T) {
	f := newRequestClaimsFixture(t)
	req := f.newRequest("assign0")
	firstOwner, secondOwner, claimant := f.insertUser("owner1"), f.insertUser("owner2"), f.insertUser("claimer")

	// Start from a request with an owner and a claim held by someone else, which
	// is the state a transfer has to resolve.
	if _, _, err := f.r.SetAssignee(f.ctx, assignWrite(req, &firstOwner)); err != nil {
		t.Fatalf("first assign: %v", err)
	}
	if _, _, err := f.r.ClaimRequest(f.ctx, f.write(req, claimant)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	before := f.mustGet(req)
	if before.ClaimUserID == nil {
		t.Fatalf("setup: the request is not claimed")
	}

	// The transfer's event must name both what it replaced and whose claim it
	// cleared; the setup above already wrote one assign event, so the transfer's is
	// the newest.
	assigned, changed, err := f.r.SetAssignee(f.ctx, assignWrite(req, &secondOwner))
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if !changed {
		t.Fatalf("the transfer reported no change")
	}
	if assigned.OwnerUserID == nil || *assigned.OwnerUserID != secondOwner {
		t.Fatalf("owner_user_id = %v, want %d", assigned.OwnerUserID, secondOwner)
	}
	if assigned.ClaimUserID != nil || assigned.ClaimedAt != nil {
		t.Fatalf("after the transfer claim = (%v, %v), want both nil: an assignment clears the claim atomically",
			assigned.ClaimUserID, assigned.ClaimedAt)
	}
	if assigned.Rev != before.Rev+1 {
		t.Errorf("rev = %d after the transfer, want %d", assigned.Rev, before.Rev+1)
	}
	events := f.events(req.ID)
	var assign *domain.RequestEvent
	assignCount := 0
	for i, ev := range events {
		if ev.Action == domain.RequestActionAssign {
			assignCount++
			assign = &events[i]
		}
	}
	if assign == nil {
		t.Fatalf("no assign event stored: a transfer writes history")
	}
	if assignCount != 2 {
		t.Fatalf("%d assign events after the setup transfer and one more, want 2", assignCount)
	}
	var detail assignEventDetail
	if err := json.Unmarshal([]byte(assign.Detail), &detail); err != nil {
		t.Fatalf("assign detail %q: %v", assign.Detail, err)
	}
	if detail.PreviousOwnerUserID == nil || *detail.PreviousOwnerUserID != firstOwner {
		t.Errorf("assign detail previous_owner_user_id = %v, want %d", detail.PreviousOwnerUserID, firstOwner)
	}
	if detail.ClearedClaimUserID == nil || *detail.ClearedClaimUserID != claimant {
		t.Errorf("assign detail cleared_claim_user_id = %v, want %d", detail.ClearedClaimUserID, claimant)
	}
	if assign.ActorUserID == nil || *assign.ActorUserID != secondOwner {
		t.Errorf("assign event actor = %v, want the caller %d", assign.ActorUserID, secondOwner)
	}

	// Replay: the row already is (secondOwner, unclaimed), so nothing moves.
	again, changed, err := f.r.SetAssignee(f.ctx, assignWrite(req, &secondOwner))
	if err != nil {
		t.Fatalf("assign replay: %v", err)
	}
	if changed {
		t.Fatalf("the assign replay reported a change")
	}
	if again.Rev != assigned.Rev {
		t.Errorf("the assign replay bumped rev to %d, want %d", again.Rev, assigned.Rev)
	}
	if got := f.actions(req.ID)[domain.RequestActionAssign]; got != 2 {
		t.Errorf("%d assign events after the replay, want 2 (the replay writes none)", got)
	}

	// A wrong tenant writes nothing at all: the assignee, the claim and the rev
	// stay exactly as the transfer above left them.
	for _, wrong := range []struct {
		name string
		w    RequestClaimWrite
	}{
		{"a sibling team in the same org", RequestClaimWrite{RequestID: req.ID, OrgID: f.org, TeamID: f.team2, Assignee: &firstOwner}},
		{"another org's team", RequestClaimWrite{RequestID: req.ID, OrgID: f.other, TeamID: f.team, Assignee: &firstOwner}},
	} {
		w := wrong.w
		w.Actor = "cross-tenant"
		if _, _, err := f.r.SetAssignee(f.ctx, w); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: err = %v, want ErrNotFound", wrong.name, err)
		}
		if _, _, err := f.r.ClaimRequest(f.ctx, RequestClaimWrite{
			RequestID: req.ID, OrgID: w.OrgID, TeamID: w.TeamID, ClaimUserID: claimant, Actor: "cross-tenant",
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: claim err = %v, want ErrNotFound", wrong.name, err)
		}
	}
	after := f.mustGet(req)
	if after.OwnerUserID == nil || *after.OwnerUserID != secondOwner || after.Rev != assigned.Rev {
		t.Fatalf("a cross-tenant write changed the row: owner=%v rev=%d, want %d at rev %d",
			after.OwnerUserID, after.Rev, secondOwner, assigned.Rev)
	}
	if got := f.actions(req.ID)[domain.RequestActionAssign]; got != 2 {
		t.Errorf("%d assign events after cross-tenant attempts, want 2", got)
	}

	// Unassigning is a real state a request can be put back into, and it is a
	// recorded transfer too.
	unassigned, changed, err := f.r.SetAssignee(f.ctx, assignWrite(req, nil))
	if err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if !changed {
		t.Fatalf("unassigning an owned request reported no change")
	}
	if unassigned.OwnerUserID != nil {
		t.Fatalf("owner_user_id = %v after unassign, want nil", unassigned.OwnerUserID)
	}
	if got := f.actions(req.ID)[domain.RequestActionAssign]; got != 3 {
		t.Errorf("%d assign events after the unassign, want 3", got)
	}
}

func assignWrite(req *domain.TeamRequest, assignee *int) RequestClaimWrite {
	w := RequestClaimWrite{
		RequestID: req.ID,
		OrgID:     req.OrgID,
		TeamID:    req.TeamID,
		Assignee:  assignee,
	}
	if assignee != nil {
		actor := fmt.Sprintf("assigner-%d", *assignee)
		w.Actor, w.ActorUserID = actor, assignee
	} else {
		w.Actor = "unassigner"
	}
	return w
}

// TestRequestClaimExpectedRevIsCompareAndSet covers the caller's precondition: a
// stale expectation is refused with the rev the row is actually at, and a current
// one writes. It also pins the rule that a replay is not a conflict — a call that
// would write nothing succeeds even with a stale expectation, because there is no
// lost update for the precondition to protect.
func TestRequestClaimExpectedRevIsCompareAndSet(t *testing.T) {
	f := newRequestClaimsFixture(t)
	req := f.newRequest("expectre")
	user, other := f.insertUser("first"), f.insertUser("second")

	w := f.write(req, user)
	w.ExpectRev = req.Rev + 5
	_, changed, err := f.r.ClaimRequest(f.ctx, w)
	var conflict *RequestRevConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("a stale expected rev: changed=%v err=%v, want *RequestRevConflict", changed, err)
	}
	if conflict.Current != req.Rev || conflict.Expected != req.Rev+5 {
		t.Fatalf("conflict = %+v, want expected %d / current %d", conflict, req.Rev+5, req.Rev)
	}
	if !errors.Is(err, ErrConflict) {
		t.Errorf("a stale rev does not match ErrConflict: %v", err)
	}
	if stored := f.mustGet(req); stored.ClaimUserID != nil || stored.Rev != req.Rev {
		t.Fatalf("a refused compare-and-set wrote: claim=%v rev=%d", stored.ClaimUserID, stored.Rev)
	}

	w.ExpectRev = req.Rev
	if _, changed, err = f.r.ClaimRequest(f.ctx, w); err != nil || !changed {
		t.Fatalf("a current expected rev: changed=%v err=%v", changed, err)
	}
	claimed := f.mustGet(req)

	// A stale expectation on a call that changes nothing is not a conflict: the
	// same claimant claiming again is a replay whatever rev they last saw.
	replay := f.write(req, user)
	replay.ExpectRev = 1
	if _, changed, err := f.r.ClaimRequest(f.ctx, replay); err != nil || changed {
		t.Fatalf("a replay with a stale expected rev: changed=%v err=%v, want a successful no-op", changed, err)
	}

	// But a release that would write does check it.
	release := f.write(req, user)
	release.ExpectRev = claimed.Rev + 4
	if _, _, err := f.r.ReleaseClaim(f.ctx, release); !errors.As(err, &conflict) {
		t.Fatalf("a release with a stale expected rev = %v, want *RequestRevConflict", err)
	}
	if stored := f.mustGet(req); stored.ClaimUserID == nil || stored.Rev != claimed.Rev {
		t.Fatalf("a refused release wrote: claim=%v rev=%d", stored.ClaimUserID, stored.Rev)
	}
	// The other user can only be given this request by a transfer, never by a claim.
	if _, _, err := f.r.ClaimRequest(f.ctx, f.write(req, other)); !errors.Is(err, ErrConflict) {
		t.Fatalf("a claim on a claimed request = %v, want a conflict", err)
	}
}

// TestRequestClaimWritesNeedScopeAndClaimant covers the refuse-a-caller-bug
// guards: a missing tenant is its own error (never a 404), a missing claimant is
// its own error (never a 409), and a claimant or assignee who is not a user is an
// ordinary not-found rather than a constraint violation escaping as a 5xx.
func TestRequestClaimWritesNeedScopeAndClaimant(t *testing.T) {
	f := newRequestClaimsFixture(t)
	req := f.newRequest("scope00")
	user := f.insertUser("holder")

	for _, tc := range []struct {
		name string
		w    RequestClaimWrite
		want error
	}{
		{"claim with no org", RequestClaimWrite{RequestID: req.ID, TeamID: f.team, ClaimUserID: user}, ErrRequestTenantScope},
		{"claim with no team", RequestClaimWrite{RequestID: req.ID, OrgID: f.org, ClaimUserID: user}, ErrRequestTenantScope},
		{"release with no tenant", RequestClaimWrite{RequestID: req.ID, ClaimUserID: user}, ErrRequestTenantScope},
		{"assign with no tenant", RequestClaimWrite{RequestID: req.ID, Assignee: &user}, ErrRequestTenantScope},
		{"a request id of zero", RequestClaimWrite{RequestID: 0, OrgID: f.org, TeamID: f.team, ClaimUserID: user}, ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, call := range []struct {
				name string
				fn   func(context.Context, RequestClaimWrite) (*domain.TeamRequest, bool, error)
			}{
				{"ClaimRequest", f.r.ClaimRequest},
				{"ReleaseClaim", f.r.ReleaseClaim},
				{"SetAssignee", f.r.SetAssignee},
			} {
				got, changed, err := call.fn(f.ctx, tc.w)
				if !errors.Is(err, tc.want) {
					t.Fatalf("%s: err = %v, want %v", call.name, err, tc.want)
				}
				if err != nil && errors.Is(err, ErrNotFound) && errors.Is(tc.want, ErrRequestTenantScope) {
					t.Fatalf("%s: a tenant-scope bug matched ErrNotFound", call.name)
				}
				if got != nil || changed {
					t.Fatalf("%s: returned (%+v, changed=%v) with an error", call.name, got, changed)
				}
			}
		})
	}

	// A claim names its claimant; 0 is "nobody", which no claim may record.
	if _, _, err := f.r.ClaimRequest(f.ctx, f.write(req, 0)); !errors.Is(err, ErrRequestClaimant) {
		t.Fatalf("a claim with no claimant = %v, want ErrRequestClaimant", err)
	}
	if _, _, err := f.r.ReleaseClaim(f.ctx, f.write(req, 0)); !errors.Is(err, ErrRequestClaimant) {
		t.Fatalf("a release with no claimant = %v, want ErrRequestClaimant", err)
	}
	if errors.Is(ErrRequestClaimant, ErrNotFound) || errors.Is(ErrRequestClaimant, ErrConflict) {
		t.Fatalf("ErrRequestClaimant must be distinct from not-found and conflict")
	}

	// A claimant or assignee who does not exist is a plain not-found for that
	// user: the request's own not-found answer must not be produced for a bad
	// user id, and a database constraint must not read as a server fault.
	missing := 1 << 30
	if _, _, err := f.r.ClaimRequest(f.ctx, f.write(req, missing)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a claim by a nonexistent user = %v, want ErrNotFound", err)
	}
	if stored := f.mustGet(req); stored.ClaimUserID != nil {
		t.Fatalf("a claim by a nonexistent user wrote claim_user_id = %v", stored.ClaimUserID)
	}
	assign := RequestClaimWrite{RequestID: req.ID, OrgID: req.OrgID, TeamID: req.TeamID, Assignee: &missing, Actor: "assigner"}
	if _, _, err := f.r.SetAssignee(f.ctx, assign); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an assignment to a nonexistent user = %v, want ErrNotFound", err)
	}
	if stored := f.mustGet(req); stored.OwnerUserID != nil || stored.Rev != req.Rev {
		t.Fatalf("an assignment to a nonexistent user wrote owner=%v rev=%d", stored.OwnerUserID, stored.Rev)
	}
	if len(f.events(req.ID)) != 0 {
		t.Fatalf("refused writes left history: %v", f.actions(req.ID))
	}
}
