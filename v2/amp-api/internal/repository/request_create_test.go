package repository

// Tests for request_create.go against a real Postgres (AMP #3871, epic 202 story
// 535; build slice C1). They run only when TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository/... -run 'CreateRequest' -count=1 -race
//
// The ticket's three criteria are what these assert, in order:
//
//  1. concurrent submissions with one idempotency key produce ONE request: every
//     caller is answered with that request, exactly one of them reports creation,
//     and the history has exactly one `submit` row (I6). The same holds for the
//     external-source uniqueness 053 already enforces — a connector replaying its
//     vendor object id concurrently creates one row.
//  2. a payload, tenant or submitter mismatch cannot reuse another request. The
//     same key with a different normalized payload is
//     *RequestIdempotencyConflict (errors.Is ErrConflict) and writes nothing; the
//     same key presented by another submitter, or against another team, is a
//     different key entirely and creates that caller's own request — it never
//     answers with a row whose content or tenant the caller is not entitled to.
//  3. a transaction that fails after the key was claimed leaves nothing behind —
//     no request, no event, no key row — so the retry with the same key succeeds
//     as a FIRST attempt. There is no pending/claimed state to get stuck in,
//     because the key row is only ever written in the transaction that commits
//     the request it names.
//
// Beyond the criteria the tests pin what the code's comments claim: a retry with a
// freshly minted public_ref still replays (the fingerprint is over the
// submission's content, not over values the service mints), a replay writes
// nothing at all (no rev bump, no second event, no second key row), a public_ref
// collision is its own actionable error and consumes no key, and validation is
// the domain's (a connector with no external id, a web submit that sends one, a
// keyed submit with no submitter, and a create with no tenant are all refused
// before anything is written).
//
// Each test builds its own org, teams and users with a random suffix, so repeated
// runs and tests in the same database don't see each other's rows.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// requestCreateFixture is requestRowsFixture plus the extra submitters a
// scope-mismatch test needs and the row counters the assertions read.
type requestCreateFixture struct {
	*requestRowsFixture
	// nextRef is atomic because a concurrency test mints a reference per attempt
	// from inside the racing goroutines.
	nextRef atomic.Int64
}

func newRequestCreateFixture(t *testing.T) *requestCreateFixture {
	t.Helper()
	return &requestCreateFixture{requestRowsFixture: newRequestRowsFixture(t)}
}

// insertUser adds one user with a run-unique subject and email and returns its id.
func (f *requestCreateFixture) insertUser(label string) int {
	f.t.Helper()
	var id int
	err := f.r.db.QueryRow(f.ctx, `INSERT INTO users (subject, email) VALUES ($1, $2) RETURNING id`,
		fmt.Sprintf("create-%s-%d", label, f.suffix),
		fmt.Sprintf("create-%s-%d@create.test", label, f.suffix)).Scan(&id)
	if err != nil {
		f.t.Fatalf("insert user %s: %v", label, err)
	}
	return id
}

// freshRef mints a public_ref this fixture owns and has not used before, so one
// test can submit the same content repeatedly without tripping 053's unique
// index. It is 16-64 characters of [A-Za-z0-9_-] like the schema requires.
func (f *requestCreateFixture) freshRef() string {
	n := f.nextRef.Add(1)
	return f.ref(fmt.Sprintf("createrequest%d", n))
}

// input is a valid web submission to this fixture's first team by this fixture's
// user. Tests vary only the field they are proving.
func (f *requestCreateFixture) input(subject string) CreateRequestInput {
	submitter := f.user
	return CreateRequestInput{
		OrgID:            f.org,
		TeamID:           f.team,
		PublicRef:        f.freshRef(),
		RequesterUserID:  &submitter,
		RequesterContact: fmt.Sprintf("requester-%d@create.test", f.suffix),
		SourceKind:       domain.RequestSourceWeb,
		Subject:          subject,
		InputBrief:       "Please price the Q3 migration.",
		Actor:            "requester",
		ActorUserID:      &submitter,
	}
}

// countRequests is how many requests this fixture's org holds. Each fixture has
// its own org, so this counts exactly the test's own submissions — including the
// ones it aimed at a second team, which is what a "nothing else was written"
// assertion has to see.
func (f *requestCreateFixture) countRequests() int {
	f.t.Helper()
	var n int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM team_requests WHERE org_id = $1`, f.org).Scan(&n); err != nil {
		f.t.Fatalf("count requests in org %d: %v", f.org, err)
	}
	return n
}

// countKeys is how many idempotency-key rows this fixture's org holds.
func (f *requestCreateFixture) countKeys() int {
	f.t.Helper()
	var n int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_idempotency_keys WHERE org_id = $1`, f.org).Scan(&n); err != nil {
		f.t.Fatalf("count idempotency keys in org %d: %v", f.org, err)
	}
	return n
}

// countEvents is how many request-history rows this fixture's org holds, whatever
// their request or action.
func (f *requestCreateFixture) countEvents() int {
	f.t.Helper()
	var n int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_events WHERE org_id = $1`, f.org).Scan(&n); err != nil {
		f.t.Fatalf("count request events in org %d: %v", f.org, err)
	}
	return n
}

// submitEvents returns every history row of a request whose action is `submit`.
func (f *requestCreateFixture) submitEvents(requestID int) []domain.RequestEvent {
	f.t.Helper()
	rows, err := f.r.db.Query(f.ctx, `SELECT id, request_id, org_id, team_id, action, from_state, to_state,
		actor, actor_user_id, detail, created_at
		FROM request_events WHERE request_id = $1 AND action = 'submit' ORDER BY id`, requestID)
	if err != nil {
		f.t.Fatalf("submit history of request %d: %v", requestID, err)
	}
	defer rows.Close()
	var out []domain.RequestEvent
	for rows.Next() {
		var ev domain.RequestEvent
		if err := rows.Scan(&ev.ID, &ev.RequestID, &ev.OrgID, &ev.TeamID, &ev.Action,
			&ev.FromStatus, &ev.ToStatus, &ev.Actor, &ev.ActorUserID, &ev.Detail, &ev.CreatedAt); err != nil {
			f.t.Fatalf("scan submit history of request %d: %v", requestID, err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("submit history of request %d: %v", requestID, err)
	}
	return out
}

// exec runs one statement on the fixture's database, for the few tests that set up
// a state only a direct write can (a failure injector, a deleted request).
func (f *requestCreateFixture) exec(sql string, args ...any) error {
	f.t.Helper()
	_, err := f.r.db.Exec(f.ctx, sql, args...)
	return err
}

// mustGet reloads a request through the shared tenant-scoped read, so assertions
// see the stored row rather than the value the write returned.
func (f *requestCreateFixture) mustGet(req *domain.TeamRequest) *domain.TeamRequest {
	f.t.Helper()
	got, err := f.r.GetRequest(f.ctx, req.ID, req.OrgID, req.TeamID)
	if err != nil {
		f.t.Fatalf("GetRequest(%d): %v", req.ID, err)
	}
	return got
}

// TestCreateRequestConcurrentSameKeyOneRequest is criterion 1. Eight callers send
// the same submission (one idempotency key, one payload) at once: exactly one
// request exists afterwards, every caller is answered with it, exactly one caller
// reports creation, and the request has exactly one `submit` history row and one
// key row.
//
// This is where both halves of the atomicity are exercised: some callers reach the
// key insert and lose on 061's unique index (errRequestIdempotencyKeyRaced, whose
// transaction rolls back the request it had just inserted), and the rest find the
// committed key on the fast path. Both are answered as a replay.
func TestCreateRequestConcurrentSameKeyOneRequest(t *testing.T) {
	f := newRequestCreateFixture(t)
	key := fmt.Sprintf("submit-key-%d", f.suffix)
	// One payload, one public_ref: the replay rule compares the payload, and a
	// fresh ref per attempt must not turn a retry into a conflict. (Separately
	// asserted below with a still-fresh ref; here every attempt sends the same
	// content because that is what a real retry does.)
	in := f.input("Concurrent replay")
	in.IdempotencyKey = key

	const n = 8
	start := make(chan struct{})
	type result struct {
		i       int
		req     *domain.TeamRequest
		created bool
		err     error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// Each goroutine sends its own fresh public_ref: the service mints one
			// per attempt, so a replay must tolerate them differing.
			attempt := in
			attempt.PublicRef = f.freshRef()
			req, created, err := f.r.CreateRequest(f.ctx, attempt)
			results[i] = result{i: i, req: req, created: created, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var creators int
	var requestID int
	var publicRef string
	var createdAt time.Time
	for _, res := range results {
		if res.err != nil {
			t.Fatalf("caller %d: %v", res.i, res.err)
		}
		if res.req == nil {
			t.Fatalf("caller %d: no request and no error", res.i)
		}
		if res.created {
			creators++
			requestID = res.req.ID
			publicRef = res.req.PublicRef
			createdAt = res.req.CreatedAt
		}
	}
	if creators != 1 {
		t.Fatalf("%d callers reported creating the request, want exactly 1", creators)
	}
	// Every caller, creator or replay, is answered with the one request: a caller
	// told "already submitted" must be given the record the other caller got, or
	// the retry has achieved nothing.
	for _, res := range results {
		if res.req.ID != requestID {
			t.Errorf("caller %d was answered with request %d, want %d", res.i, res.req.ID, requestID)
		}
		if res.req.PublicRef != publicRef {
			t.Errorf("caller %d was answered with public_ref %q, want %q", res.i, res.req.PublicRef, publicRef)
		}
	}
	if got := f.countRequests(); got != 1 {
		t.Fatalf("team has %d requests after %d concurrent same-key submissions, want 1", got, n)
	}
	if got := f.countKeys(); got != 1 {
		t.Fatalf("team has %d idempotency-key rows, want 1", got)
	}
	stored := f.mustGet(&domain.TeamRequest{ID: requestID, OrgID: f.org, TeamID: f.team})
	if stored.PublicRef != publicRef {
		t.Errorf("stored public_ref = %q, want the creator's %q", stored.PublicRef, publicRef)
	}
	if !stored.CreatedAt.Equal(createdAt) {
		t.Errorf("stored created_at = %s, want the creator's %s", stored.CreatedAt.UTC(), createdAt.UTC())
	}
	if stored.State != domain.RequestStatusNew || stored.Rev != 1 {
		t.Errorf("stored state/rev = %s/%d, want new/1", stored.State, stored.Rev)
	}
	if events := f.submitEvents(requestID); len(events) != 1 {
		t.Fatalf("%d submit history rows, want exactly 1 (one act, one row)", len(events))
	}
}

// TestCreateRequestConcurrentWinnersPayloadConflicts pins the other half of a
// same-key race: when the racing callers disagree about the payload, exactly one
// payload wins and every caller that sent the other one is refused as a conflict —
// never answered with content they did not send.
func TestCreateRequestConcurrentWinnersPayloadConflicts(t *testing.T) {
	f := newRequestCreateFixture(t)
	key := fmt.Sprintf("race-key-%d", f.suffix)

	base := f.input("Race A")
	base.IdempotencyKey = key
	other := base
	other.Subject = "Race B"

	const perSide = 4
	type result struct {
		subject string
		created bool
		req     *domain.TeamRequest
		err     error
	}
	results := make(chan result, perSide*2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < perSide; i++ {
		for _, in := range []CreateRequestInput{base, other} {
			wg.Add(1)
			go func(in CreateRequestInput) {
				defer wg.Done()
				<-start
				in.PublicRef = f.freshRef()
				req, created, err := f.r.CreateRequest(f.ctx, in)
				results <- result{subject: in.Subject, created: created, req: req, err: err}
			}(in)
		}
	}
	close(start)
	wg.Wait()
	close(results)

	var winner string
	var winnerID int
	for res := range results {
		switch {
		case res.err == nil && res.created:
			if winner != "" && winner != res.subject {
				t.Fatalf("two payloads both won the key: %q and %q", winner, res.subject)
			}
			winner, winnerID = res.subject, res.req.ID
		case res.err == nil:
			// A replay: its payload must be the winner's.
			if res.req.Subject != winner && winner != "" {
				t.Fatalf("a replay of %q was answered with a request for %q", res.subject, res.req.Subject)
			}
		case errors.Is(res.err, ErrConflict):
			var conflict *RequestIdempotencyConflict
			if !errors.As(res.err, &conflict) {
				t.Fatalf("conflict is %T, want *RequestIdempotencyConflict: %v", res.err, res.err)
			}
			if winner != "" && res.subject == winner {
				t.Fatalf("caller sent the winning payload %q and was refused", res.subject)
			}
		default:
			t.Fatalf("caller with payload %q: %v", res.subject, res.err)
		}
	}
	if winner == "" {
		t.Fatal("no caller won the key")
	}
	if got := f.countRequests(); got != 1 {
		t.Fatalf("%d requests after a contested same-key race, want 1", got)
	}
	stored := f.mustGet(&domain.TeamRequest{ID: winnerID, OrgID: f.org, TeamID: f.team})
	if stored.Subject != winner {
		t.Errorf("stored subject = %q, want the winner's %q", stored.Subject, winner)
	}
}

// TestCreateRequestScopeCannotReuseAnotherRequest is criterion 2, as a matrix.
// Each row is a way a caller could try to reach a request that is not theirs, and
// the answer it must get.
func TestCreateRequestScopeCannotReuseAnotherRequest(t *testing.T) {
	f := newRequestCreateFixture(t)
	first := f.input("The first requester's request")
	first.IdempotencyKey = fmt.Sprintf("shared-key-%d", f.suffix)

	orig, created, err := f.r.CreateRequest(f.ctx, first)
	if err != nil || !created {
		t.Fatalf("first submission: created=%v err=%v", created, err)
	}

	t.Run("a different payload on the same key is a conflict and changes nothing", func(t *testing.T) {
		before := f.mustGet(orig)
		changed := first
		changed.PublicRef = f.freshRef()
		changed.InputBrief = "A different brief entirely."
		got, created, err := f.r.CreateRequest(f.ctx, changed)
		if got != nil || created {
			t.Fatalf("a different payload returned request %v (created=%v); a keyed conflict writes nothing", got, created)
		}
		var conflict *RequestIdempotencyConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("err = %v, want *RequestIdempotencyConflict", err)
		}
		if !errors.Is(err, ErrConflict) {
			t.Errorf("err = %v, want it to match ErrConflict so a handler answers 409", err)
		}
		if conflict.RequestID != orig.ID {
			t.Errorf("conflict names request %d, want %d", conflict.RequestID, orig.ID)
		}
		if conflict.Submitter != f.user {
			t.Errorf("conflict names submitter %d, want %d", conflict.Submitter, f.user)
		}
		after := f.mustGet(orig)
		if after.InputBrief != before.InputBrief || after.Subject != before.Subject || after.Rev != before.Rev {
			t.Errorf("a refused keyed submission changed the stored request: %+v -> %+v", before, after)
		}
		if got := f.countRequests(); got != 1 {
			t.Errorf("%d requests after a refused submission, want 1", got)
		}
	})

	t.Run("normalization does not make an equal payload conflict", func(t *testing.T) {
		// Same content, padded with whitespace the domain trims: a retry of the
		// same submission, not a different one.
		padded := first
		padded.PublicRef = f.freshRef()
		padded.Subject = "   " + first.Subject + "  "
		padded.InputBrief = "\n" + first.InputBrief + "\t"
		got, created, err := f.r.CreateRequest(f.ctx, padded)
		if err != nil {
			t.Fatalf("a whitespace-only difference was refused: %v", err)
		}
		if created {
			t.Fatalf("a whitespace-only difference created a second request %d, want a replay of %d", got.ID, orig.ID)
		}
		if got.ID != orig.ID {
			t.Fatalf("replay returned request %d, want %d", got.ID, orig.ID)
		}
		if got.Rev != orig.Rev {
			t.Errorf("replay bumped rev to %d, want it left at %d", got.Rev, orig.Rev)
		}
		if n := len(f.submitEvents(orig.ID)); n != 1 {
			t.Errorf("%d submit history rows after a replay, want 1", n)
		}
	})

	t.Run("the same key by another submitter creates that submitter's own request", func(t *testing.T) {
		other := f.insertUser("othersubmitter")
		in := f.input("Another person's request")
		in.RequesterUserID = &other
		in.ActorUserID = &other
		in.IdempotencyKey = first.IdempotencyKey
		in.RequesterContact = fmt.Sprintf("other-%d@create.test", f.suffix)

		got, created, err := f.r.CreateRequest(f.ctx, in)
		if err != nil {
			t.Fatalf("another submitter's submission: %v", err)
		}
		if !created {
			t.Fatalf("another submitter's submission replayed request %d: one person's key must not answer for another", got.ID)
		}
		if got.ID == orig.ID {
			t.Fatalf("another submitter was answered with request %d, which belongs to user %d", orig.ID, f.user)
		}
		if got.RequesterUserID == nil || *got.RequesterUserID != other {
			t.Fatalf("the new request's requester = %v, want the second submitter %d", got.RequesterUserID, other)
		}
		if got.Subject != in.Subject {
			t.Fatalf("the new request's subject = %q, want %q", got.Subject, in.Subject)
		}
		if got := f.countKeys(); got != 2 {
			t.Errorf("%d key rows, want 2 (one per submitter, same token)", got)
		}
	})

	t.Run("the same key in another team creates that team's request", func(t *testing.T) {
		in := f.input("A request for the second desk")
		in.TeamID = f.team2
		in.IdempotencyKey = first.IdempotencyKey
		got, created, err := f.r.CreateRequest(f.ctx, in)
		if err != nil {
			t.Fatalf("submission to the second team: %v", err)
		}
		if !created {
			t.Fatalf("the same key in another team replayed request %d from team %d", got.ID, orig.TeamID)
		}
		if got.ID == orig.ID {
			t.Fatalf("another team was answered with request %d from team %d", orig.ID, orig.TeamID)
		}
		if got.TeamID != f.team2 {
			t.Fatalf("the new request's team = %d, want %d", got.TeamID, f.team2)
		}
		// The tenant-scoped read agrees: the first team's request is invisible to
		// the second team, so the key could not have answered with it anyway.
		if _, err := f.r.GetRequest(f.ctx, orig.ID, f.org, f.team2); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetRequest across teams = %v, want ErrNotFound", err)
		}
	})

	t.Run("the same key on another source creates that source's request", func(t *testing.T) {
		in := f.input("Submitted over MCP")
		in.SourceKind = domain.RequestSourceMCP
		in.IdempotencyKey = first.IdempotencyKey
		got, created, err := f.r.CreateRequest(f.ctx, in)
		if err != nil {
			t.Fatalf("MCP submission: %v", err)
		}
		if !created || got.ID == orig.ID {
			t.Fatalf("a key on another source replayed the web request: created=%v id=%d (orig %d)", created, got.ID, orig.ID)
		}
		if got.SourceKind != domain.RequestSourceMCP {
			t.Errorf("source_kind = %q, want mcp", got.SourceKind)
		}
	})
}

// TestCreateRequestConnectorExternalIDConcurrent is the second half of criterion
// 1: 053's partial unique index already makes a connector's replay a no-op, and
// concurrent same-external-id creates must produce one row — with each loser
// answered with the winner's request rather than an error.
func TestCreateRequestConnectorExternalIDConcurrent(t *testing.T) {
	f := newRequestCreateFixture(t)
	externalID := fmt.Sprintf("gh-issue-%d", f.suffix)

	mk := func() CreateRequestInput {
		in := f.input("Imported from GitHub")
		in.SourceKind = domain.RequestSourceConnector
		in.SourceExternalID = externalID
		in.RequesterUserID = nil
		in.Actor = fmt.Sprintf("connector-%d", f.suffix)
		in.ActorUserID = nil
		in.RequesterContact = fmt.Sprintf("bot-%d@create.test", f.suffix)
		in.PublicRef = f.freshRef()
		return in
	}

	const n = 6
	type result struct {
		i       int
		req     *domain.TeamRequest
		created bool
		err     error
	}
	results := make(chan result, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req, created, err := f.r.CreateRequest(f.ctx, mk())
			results <- result{i: i, req: req, created: created, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	var creators int
	var id int
	for res := range results {
		if res.err != nil {
			t.Fatalf("connector %d: %v", res.i, res.err)
		}
		if res.created {
			creators++
			id = res.req.ID
		} else if res.req == nil || res.req.ID != id && id != 0 {
			t.Fatalf("connector %d: replay answered with %v, want request %d", res.i, res.req, id)
		}
	}
	if creators != 1 {
		t.Fatalf("%d connector submissions created a request, want exactly 1", creators)
	}
	if got := f.countRequests(); got != 1 {
		t.Fatalf("%d requests for one external id, want 1", got)
	}
	if got := f.countKeys(); got != 0 {
		t.Fatalf("%d idempotency-key rows were written by connector submissions that sent no key", got)
	}
	stored := f.mustGet(&domain.TeamRequest{ID: id, OrgID: f.org, TeamID: f.team})
	if stored.SourceExternalID != externalID {
		t.Errorf("source_external_id = %q, want %q", stored.SourceExternalID, externalID)
	}
	if stored.RequesterUserID != nil {
		t.Errorf("requester_user_id = %v, want nil for a connector submission", stored.RequesterUserID)
	}

	// A later connector replay (no concurrency at all) is the same answer, and it
	// writes nothing: no second request, no second event.
	again, created, err := f.r.CreateRequest(f.ctx, mk())
	if err != nil {
		t.Fatalf("connector replay: %v", err)
	}
	if created {
		t.Fatalf("a connector replay created a second request %d", again.ID)
	}
	if again.ID != id {
		t.Fatalf("connector replay returned request %d, want %d", again.ID, id)
	}
	if n := len(f.submitEvents(id)); n != 1 {
		t.Errorf("%d submit history rows after a connector replay, want 1", n)
	}
}

// TestCreateRequestFailedTransactionRetrySucceeds is criterion 3, and it is
// literal: a trigger makes the transaction fail AFTER the request row and the key
// row were written (an AFTER INSERT trigger on request_idempotency_keys). The
// assertion is that the failure leaves nothing at all behind — so the retry with
// the same key is a first attempt, not a replay of a request that never committed
// and not a "claim" it has to wait out or steal.
func TestCreateRequestFailedTransactionRetrySucceeds(t *testing.T) {
	f := newRequestCreateFixture(t)
	marker := fmt.Sprintf("fail-%d", f.suffix)

	// The injector. It fires only for this test's marker, so no other test in the
	// database can be affected even if the cleanup below ever failed.
	if _, err := f.r.db.Exec(f.ctx, `
		CREATE OR REPLACE FUNCTION request_create_test_fail_key() RETURNS trigger AS $$
		BEGIN
			IF NEW.idempotency_key = TG_ARGV[0] THEN
				RAISE EXCEPTION 'injected failure after the request and key rows were written';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create the failure injector: %v", err)
	}
	dropInjector := func() {
		f.t.Helper()
		if _, err := f.r.db.Exec(f.ctx, `DROP TRIGGER IF EXISTS request_create_test_fail_key ON request_idempotency_keys`); err != nil {
			f.t.Fatalf("drop the failure injector trigger: %v", err)
		}
		if _, err := f.r.db.Exec(f.ctx, `DROP FUNCTION IF EXISTS request_create_test_fail_key()`); err != nil {
			f.t.Fatalf("drop the failure injector function: %v", err)
		}
	}
	f.t.Cleanup(dropInjector)
	// The trigger argument is a literal (a trigger definition takes no bind
	// parameters); marker is generated by this test, never user input.
	install := fmt.Sprintf(`CREATE TRIGGER request_create_test_fail_key
		AFTER INSERT ON request_idempotency_keys FOR EACH ROW
		EXECUTE FUNCTION request_create_test_fail_key('%s')`, marker)
	if _, err := f.r.db.Exec(f.ctx, install); err != nil {
		t.Fatalf("install the failure injector: %v", err)
	}

	in := f.input("A submission whose transaction fails")
	in.IdempotencyKey = marker // the injector's trigger argument is the key itself

	req, created, err := f.r.CreateRequest(f.ctx, in)
	if err == nil {
		t.Fatalf("the injected failure produced no error (request %v, created=%v)", req, created)
	}
	if req != nil || created {
		t.Fatalf("a failed transaction returned request %v (created=%v)", req, created)
	}
	// Nothing survived: no request row, no history row, no key row. This is what
	// "no stuck claim" means — there is no pending row to expire, release or take
	// over, because the key row only exists in a transaction that commits.
	if got := f.countRequests(); got != 0 {
		t.Fatalf("%d request rows survived a failed transaction, want 0", got)
	}
	if got := f.countKeys(); got != 0 {
		t.Fatalf("%d key rows survived a failed transaction, want 0 (that is the stuck claim)", got)
	}
	if events := f.countEvents(); events != 0 {
		t.Fatalf("%d history rows survived a failed transaction, want 0", events)
	}

	// The retry the client makes, with the same key and the same content, after
	// the transient fault is gone.
	dropInjector()
	first, created, err := f.r.CreateRequest(f.ctx, in)
	if err != nil {
		t.Fatalf("retry after a failed transaction: %v", err)
	}
	if !created {
		t.Fatalf("the retry was answered as a replay of request %d, but the failed attempt wrote nothing", first.ID)
	}
	if got := f.countRequests(); got != 1 {
		t.Fatalf("%d requests after the retry, want 1", got)
	}
	if got := f.countKeys(); got != 1 {
		t.Fatalf("%d key rows after the retry, want 1", got)
	}
	if n := len(f.submitEvents(first.ID)); n != 1 {
		t.Fatalf("%d submit history rows after the retry, want 1", n)
	}

	// And the key now replays: the retry itself is complete, so a third attempt
	// is the replay the key was for.
	second, created, err := f.r.CreateRequest(f.ctx, in)
	if err != nil {
		t.Fatalf("replay after the successful retry: %v", err)
	}
	if created || second.ID != first.ID {
		t.Fatalf("a replay after the retry = id %d created=%v, want request %d as a replay", second.ID, created, first.ID)
	}
	if got := f.countRequests(); got != 1 {
		t.Fatalf("%d requests after the replay, want 1", got)
	}
}

// TestCreateRequestRefusals covers the answers that are errors because a
// submission is wrong rather than a repeat: they all refuse before anything is
// written, and each keeps its own error identity so a handler can tell a 400 from
// a 404 from a 500.
func TestCreateRequestRefusals(t *testing.T) {
	f := newRequestCreateFixture(t)

	t.Run("a keyed submission with no submitter", func(t *testing.T) {
		in := f.input("Anonymous keyed submit")
		in.RequesterUserID = nil
		in.IdempotencyKey = fmt.Sprintf("anon-%d", f.suffix)
		if _, _, err := f.r.CreateRequest(f.ctx, in); !errors.Is(err, ErrRequestSubmitter) {
			t.Fatalf("err = %v, want ErrRequestSubmitter", err)
		}
	})

	t.Run("no tenant", func(t *testing.T) {
		in := f.input("No team")
		in.TeamID = 0
		if _, _, err := f.r.CreateRequest(f.ctx, in); !errors.Is(err, ErrRequestTenantScope) {
			t.Fatalf("err = %v, want ErrRequestTenantScope", err)
		}
	})

	t.Run("a public_ref already in use", func(t *testing.T) {
		first := f.input("Owns the reference")
		if _, created, err := f.r.CreateRequest(f.ctx, first); err != nil || !created {
			t.Fatalf("first submission: created=%v err=%v", created, err)
		}
		// The same token, in another team's request: public_ref is unique across
		// the whole table (053), so this is a collision even though nothing else
		// is shared.
		clash := f.input("Wants the same reference")
		clash.TeamID = f.team2
		clash.PublicRef = first.PublicRef
		clash.IdempotencyKey = fmt.Sprintf("clash-%d", f.suffix)
		got, created, err := f.r.CreateRequest(f.ctx, clash)
		if created || got != nil {
			t.Fatalf("a colliding public_ref produced request %v (created=%v)", got, created)
		}
		var refErr *RequestPublicRefConflict
		if !errors.As(err, &refErr) {
			t.Fatalf("err = %v, want *RequestPublicRefConflict", err)
		}
		if refErr.PublicRef != first.PublicRef {
			t.Errorf("the refusal names %q, want %q", refErr.PublicRef, first.PublicRef)
		}
		if errors.Is(err, ErrConflict) {
			t.Errorf("a public_ref collision matches ErrConflict; it is the caller's to retry with a new token, not a 409")
		}
		if got := f.countKeys(); got != 0 {
			t.Fatalf("%d key rows after the collision, want 0: the key must not be consumed by a failed attempt", got)
		}
		// The same key, a fresh reference: it now works, because the failed attempt
		// left nothing behind.
		clash.PublicRef = f.freshRef()
		if _, created, err := f.r.CreateRequest(f.ctx, clash); err != nil || !created {
			t.Fatalf("retry with a fresh public_ref: created=%v err=%v", created, err)
		}
	})

	t.Run("a connector with no external id", func(t *testing.T) {
		in := f.input("Connector without its object id")
		in.SourceKind = domain.RequestSourceConnector
		in.SourceExternalID = ""
		if _, _, err := f.r.CreateRequest(f.ctx, in); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want a domain validation error naming source_external_id", err)
		}
	})

	t.Run("a web submission that sends an external id", func(t *testing.T) {
		in := f.input("Web submit with a vendor id")
		in.SourceExternalID = "gh-1"
		if _, _, err := f.r.CreateRequest(f.ctx, in); err == nil {
			t.Fatal("a web submission carrying a source_external_id was accepted")
		}
	})

	t.Run("a blank subject", func(t *testing.T) {
		in := f.input("   ")
		if _, _, err := f.r.CreateRequest(f.ctx, in); err == nil {
			t.Fatal("a blank subject was accepted")
		}
	})

	t.Run("a submitter who does not exist", func(t *testing.T) {
		in := f.input("Ghost submitter")
		ghost := 1 << 30
		in.RequesterUserID = &ghost
		in.IdempotencyKey = fmt.Sprintf("ghost-%d", f.suffix)
		if _, _, err := f.r.CreateRequest(f.ctx, in); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound for the user", err)
		}
		if got := f.countKeys(); got != 1 {
			t.Fatalf("%d key rows, want the 1 from the earlier successful retry and nothing from the ghost", got)
		}
	})

	if got := f.countRequests(); got != 2 {
		t.Fatalf("%d requests after the refusal matrix, want 2 (one per successful submission)", got)
	}
}

// TestCreateRequestStoresTheSubmission pins what the create writes on the happy
// path — the record starts as `new` at rev 1 with the contact snapshot, the
// metadata cleaned, and exactly one `submit` history row whose detail names the
// source — so a later slice that reads intake can rely on it.
func TestCreateRequestStoresTheSubmission(t *testing.T) {
	f := newRequestCreateFixture(t)
	submitter := f.user
	in := f.input("Store me exactly")
	in.Metadata = domain.Metadata{
		"jira_key":  "PLAT-12",
		"severity":  3,
		"escalated": true,
	}
	in.IdempotencyKey = fmt.Sprintf("store-%d", f.suffix)

	req, created, err := f.r.CreateRequest(f.ctx, in)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	stored := f.mustGet(req)
	if stored.State != domain.RequestStatusNew {
		t.Errorf("state = %q, want new", stored.State)
	}
	if stored.Rev != 1 {
		t.Errorf("rev = %d, want 1", stored.Rev)
	}
	if stored.ClaimUserID != nil || stored.OwnerUserID != nil || stored.ClaimedAt != nil {
		t.Errorf("a new request carries a claim/assignee: claim=%v owner=%v", stored.ClaimUserID, stored.OwnerUserID)
	}
	if stored.RequesterContact != in.RequesterContact {
		t.Errorf("requester_contact = %q, want the snapshot %q", stored.RequesterContact, in.RequesterContact)
	}
	if stored.RequesterUserID == nil || *stored.RequesterUserID != submitter {
		t.Errorf("requester_user_id = %v, want %d", stored.RequesterUserID, submitter)
	}
	if got := stored.Metadata["jira_key"]; got != "PLAT-12" {
		t.Errorf("metadata jira_key = %v, want PLAT-12", got)
	}
	// JSONB numbers come back as float64 (the request_rows contract's gotcha).
	if got := stored.Metadata["severity"]; got != float64(3) {
		t.Errorf("metadata severity = %#v, want float64(3)", got)
	}
	if got := stored.Metadata["escalated"]; got != true {
		t.Errorf("metadata escalated = %#v, want true", got)
	}
	if stored.CreatedAt.IsZero() || !stored.CreatedAt.Equal(stored.UpdatedAt) {
		t.Errorf("created_at/updated_at = %s/%s, want one NOW()", stored.CreatedAt, stored.UpdatedAt)
	}

	events := f.submitEvents(req.ID)
	if len(events) != 1 {
		t.Fatalf("%d submit history rows, want 1", len(events))
	}
	ev := events[0]
	if ev.OrgID != f.org || ev.TeamID != f.team {
		t.Errorf("history row tenant = %d/%d, want %d/%d", ev.OrgID, ev.TeamID, f.org, f.team)
	}
	if ev.FromStatus != "" || ev.ToStatus != "" {
		t.Errorf("a submit names states (%q -> %q); 054 allows states on a transition only", ev.FromStatus, ev.ToStatus)
	}
	if ev.Actor != "requester" {
		t.Errorf("history actor = %q, want the caller's requester", ev.Actor)
	}
	if ev.ActorUserID == nil || *ev.ActorUserID != submitter {
		t.Errorf("history actor_user_id = %v, want %d", ev.ActorUserID, submitter)
	}
	if ev.Detail == "" {
		t.Error("the submit history row carries no detail; it should name the source")
	}

	// Running the same submission through the shared read/write contract proves the
	// create wrote nothing else: no comments, no other events.
	var comments int
	if err := f.r.db.QueryRow(f.ctx, `SELECT count(*) FROM request_comments WHERE request_id = $1`, req.ID).Scan(&comments); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if comments != 0 {
		t.Errorf("%d comment rows after a create, want 0", comments)
	}
}

// TestCreateRequestUnkeyedSubmitsAreDistinct is the negative guard on the whole
// mechanism: without a key (and without an external id) every submission is its
// own request. Deduplicating unkeyed web submits against each other would silently
// swallow a team's real second request.
func TestCreateRequestUnkeyedSubmitsAreDistinct(t *testing.T) {
	f := newRequestCreateFixture(t)
	var ids []int
	for i := 0; i < 3; i++ {
		in := f.input("The same words, sent again")
		req, created, err := f.r.CreateRequest(f.ctx, in)
		if err != nil {
			t.Fatalf("submission %d: %v", i, err)
		}
		if !created {
			t.Fatalf("submission %d was deduplicated (request %d); an unkeyed submit is a new request", i, req.ID)
		}
		ids = append(ids, req.ID)
	}
	if got := f.countRequests(); got != 3 {
		t.Fatalf("%d requests after three unkeyed submissions, want 3", got)
	}
	if got := f.countKeys(); got != 0 {
		t.Fatalf("%d key rows were written for submissions that sent no key", got)
	}
	seen := map[int]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("request id %d was returned twice", id)
		}
		seen[id] = true
	}
}

// TestCreateRequestSequentialReplayWritesNothing pins the replay path's
// no-write property against the stored row: the same key and payload returns the
// ORIGINAL row (its public_ref and created_at, not the retry's freshly minted
// ones), with no rev bump, no second event and no second key row.
func TestCreateRequestSequentialReplayWritesNothing(t *testing.T) {
	f := newRequestCreateFixture(t)
	in := f.input("Replay me")
	in.IdempotencyKey = fmt.Sprintf("replay-%d", f.suffix)

	orig, created, err := f.r.CreateRequest(f.ctx, in)
	if err != nil || !created {
		t.Fatalf("first submission: created=%v err=%v", created, err)
	}
	before := f.mustGet(orig)

	for i := 0; i < 3; i++ {
		retry := in
		retry.PublicRef = f.freshRef() // the service mints a new token per attempt
		retry.Metadata = domain.Metadata{"attempt": fmt.Sprint(i + 1)}
		// metadata is part of the payload, so this retry is honestly a different
		// one; use the original metadata to replay.
		retry.Metadata = in.Metadata
		got, created, err := f.r.CreateRequest(f.ctx, retry)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if created {
			t.Fatalf("replay %d created a new request %d", i, got.ID)
		}
		if got.ID != orig.ID {
			t.Fatalf("replay %d returned request %d, want %d", i, got.ID, orig.ID)
		}
		if got.PublicRef != before.PublicRef {
			t.Errorf("replay %d returned public_ref %q, want the original %q", i, got.PublicRef, before.PublicRef)
		}
		if !got.CreatedAt.Equal(before.CreatedAt) || !got.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("replay %d moved created_at/updated_at: %s/%s want %s/%s", i,
				got.CreatedAt.UTC(), got.UpdatedAt.UTC(), before.CreatedAt.UTC(), before.UpdatedAt.UTC())
		}
		if got.Rev != before.Rev {
			t.Errorf("replay %d bumped rev to %d, want %d", i, got.Rev, before.Rev)
		}
	}
	if got := f.countRequests(); got != 1 {
		t.Fatalf("%d requests after a replay, want 1", got)
	}
	if got := f.countKeys(); got != 1 {
		t.Fatalf("%d key rows after a replay, want 1", got)
	}
	if n := len(f.submitEvents(orig.ID)); n != 1 {
		t.Fatalf("%d submit history rows after replays, want 1", n)
	}
}

// TestCreateRequestKeyDeletedWithItsRequest is the retention rule 061 states: a
// key row follows the request it names (ON DELETE CASCADE), so a retry after a
// request is deleted creates a fresh request instead of being answered with an id
// that no longer exists.
func TestCreateRequestKeyDeletedWithItsRequest(t *testing.T) {
	f := newRequestCreateFixture(t)
	in := f.input("Delete and resubmit")
	in.IdempotencyKey = fmt.Sprintf("cascade-%d", f.suffix)

	orig, created, err := f.r.CreateRequest(f.ctx, in)
	if err != nil || !created {
		t.Fatalf("first submission: created=%v err=%v", created, err)
	}
	if err := f.exec(`DELETE FROM team_requests WHERE id = $1`, orig.ID); err != nil {
		t.Fatalf("delete the request: %v", err)
	}
	if got := f.countKeys(); got != 0 {
		t.Fatalf("%d key rows survived their request's deletion, want 0", got)
	}
	retry := in
	retry.PublicRef = f.freshRef()
	got, created, err := f.r.CreateRequest(f.ctx, retry)
	if err != nil {
		t.Fatalf("resubmit after deletion: %v", err)
	}
	if !created || got.ID == orig.ID {
		t.Fatalf("resubmit after deletion = id %d created=%v, want a new request", got.ID, created)
	}
}

// TestCreateRequestWithinContextCancelled proves a cancelled caller is refused as
// itself rather than being read as a replay: a dead context must not make a
// submission look like it succeeded.
func TestCreateRequestWithinContextCancelled(t *testing.T) {
	f := newRequestCreateFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	in := f.input("Never sent")
	in.IdempotencyKey = fmt.Sprintf("cancelled-%d", f.suffix)
	req, created, err := f.r.CreateRequest(ctx, in)
	if err == nil {
		t.Fatalf("a cancelled create returned request %v (created=%v)", req, created)
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("err = %v, want the context's own error", err)
	}
	if got := f.countRequests(); got != 0 {
		t.Fatalf("%d requests were written by a cancelled create, want 0", got)
	}
}
