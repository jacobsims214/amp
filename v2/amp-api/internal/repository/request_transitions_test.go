package repository

// Tests for request_transitions.go against a real Postgres (AMP #3960, epic 202
// story 548; build slices S5/S6's write half). They run only when
// TEST_DATABASE_URL is set:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository -run 'ApplyRequestTransition|RequestPriorState|RequestLatestQuestion' \
//	    -count=1 -race
//
// The ticket's criteria are what these assert:
//
//  1. state, revision and the history row are ONE write: a move that applies
//     changes exactly those three things (plus the comment and columns its plan
//     names), and a move that fails at any point in the transaction — an
//     injected failure on the history insert, an answer naming no question, a
//     delivery with no link — leaves NOTHING behind: same state, same rev, no
//     comment, no event.
//  2. the question/answer pair is structural: entering needs_information without
//     a question cannot be written through this path (the shape check refuses
//     it), the question comment lands in the same transaction as the state, and
//     an answer is accepted only when answer_to names a `question` comment of
//     this request — a positive id that names nothing, another request's comment,
//     or a non-question comment is refused and writes nothing.
//  3. the row a decision was made on is the row written: a stale revision is
//     RequestRevConflict and a state that moved is RequestStateConflict, both
//     matching ErrConflict, both writing nothing.
//
// Beyond the criteria the tests pin what the code claims: a delivery is refused
// without a delivered_by relation unless the explicit no-artifact record is set
// (and is allowed with either); the first-write-wins entry timestamps do not
// move on a second entry while reopened_count counts every reopen; a reopen out
// of duplicate detaches the pointer AND the live relation row in the same write
// while the fold stays in the history; the columns the policy can name are all
// writable here; the prior state comes from the history and not from anywhere
// else; and a foreign tenant writes nothing and reads as not-found.
//
// Each test builds its own org, teams, users, project and requests with a random
// suffix (requestLinksFixture), so repeated runs and tests in the same database
// don't see each other's rows. The injected failures use PostgreSQL triggers
// scoped to this fixture's own request id (the request_edits_test.go pattern), so
// the real INSERT is the statement that fails.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
)

// requestTransitionsFixture is requestLinksFixture plus the readers and writers
// this file's assertions need.
type requestTransitionsFixture struct {
	*requestLinksFixture
}

func newRequestTransitionsFixture(t *testing.T) *requestTransitionsFixture {
	t.Helper()
	return &requestTransitionsFixture{requestLinksFixture: newRequestLinksFixture(t)}
}

// transitionWrite is the caller's part of one move for this fixture's tenant,
// with the actor set. Tests vary the fields they care about.
func (f *requestTransitionsFixture) transitionWrite(req *domain.TeamRequest, to domain.RequestStatus) RequestTransitionWrite {
	return RequestTransitionWrite{
		RequestID: req.ID,
		OrgID:     f.org,
		TeamID:    req.TeamID,
		From:      req.State,
		To:        to,
		ExpectRev: req.Rev,
		Actor:     "triager@transitions.test",
		ActorUserID: func() *int {
			id := f.user
			return &id
		}(),
	}
}

// comments reads the request's thread in append order, straight from the table.
func (f *requestTransitionsFixture) comments(requestID int) []domain.RequestComment {
	f.t.Helper()
	rows, err := f.r.db.Query(f.ctx, `SELECT `+requestCommentCols("")+` FROM request_comments
		WHERE request_id = $1 ORDER BY id`, requestID)
	if err != nil {
		f.t.Fatalf("thread of request %d: %v", requestID, err)
	}
	defer rows.Close()
	out := []domain.RequestComment{}
	for rows.Next() {
		var c domain.RequestComment
		if err := scanRequestComment(rows, &c); err != nil {
			f.t.Fatalf("scan comment of request %d: %v", requestID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("thread of request %d: %v", requestID, err)
	}
	return out
}

// mustMove applies a move and fails the test if it does not succeed.
func (f *requestTransitionsFixture) mustMove(w RequestTransitionWrite) (*domain.TeamRequest, *domain.RequestComment) {
	f.t.Helper()
	req, comment, err := f.r.ApplyRequestTransition(f.ctx, w)
	if err != nil {
		f.t.Fatalf("ApplyRequestTransition(%s → %s): %v", w.From, w.To, err)
	}
	return req, comment
}

// assertNothingMoved proves a refused write left the row, its thread and its
// history exactly as they were.
func (f *requestTransitionsFixture) assertNothingMoved(req *domain.TeamRequest, wantComments, wantEvents int) {
	f.t.Helper()
	stored, err := f.r.GetRequest(f.ctx, req.ID, f.org, req.TeamID)
	if err != nil {
		f.t.Fatalf("re-read request %d: %v", req.ID, err)
	}
	if stored.State != req.State || stored.Rev != req.Rev {
		f.t.Errorf("a refused write moved the row: state %s rev %d, want %s rev %d",
			stored.State, stored.Rev, req.State, req.Rev)
	}
	if n := len(f.comments(req.ID)); n != wantComments {
		f.t.Errorf("a refused write left %d comments, want %d", n, wantComments)
	}
	if n := len(f.events(req.ID)); n != wantEvents {
		f.t.Errorf("a refused write left %d history rows, want %d", n, wantEvents)
	}
}

// TestApplyRequestTransitionWritesStateRevAndOneEvent is the first criterion: one
// move changes the state, bumps the revision exactly once, stamps the column its
// plan names and appends exactly one transition history row — all of it, or none.
func TestApplyRequestTransitionWritesStateRevAndOneEvent(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans000", domain.RequestStatusNew)

	w := f.transitionWrite(req, domain.RequestStatusTriaging)
	got, comment := f.mustMove(w)
	if comment != nil {
		t.Errorf("a plain triage step appended a comment %+v", comment)
	}
	if got.State != domain.RequestStatusTriaging {
		t.Errorf("state = %s, want triaging", got.State)
	}
	if got.Rev != req.Rev+1 {
		t.Errorf("rev = %d, want %d", got.Rev, req.Rev+1)
	}

	// The returned row is the stored row, not a locally patched guess.
	stored := f.mustGet(req)
	if stored.State != got.State || stored.Rev != got.Rev {
		t.Errorf("returned row %s@%d differs from stored %s@%d", got.State, got.Rev, stored.State, stored.Rev)
	}
	events := f.events(req.ID)
	if len(events) != 1 {
		t.Fatalf("history rows = %d, want exactly one: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Action != domain.RequestActionTransition {
		t.Errorf("history action = %q, want transition", ev.Action)
	}
	if ev.FromStatus != domain.RequestStatusNew || ev.ToStatus != domain.RequestStatusTriaging {
		t.Errorf("history states = %s → %s, want new → triaging", ev.FromStatus, ev.ToStatus)
	}
	if ev.Actor != "triager@transitions.test" {
		t.Errorf("history actor = %q, want the caller's label", ev.Actor)
	}

	// A second move appends a second row and bumps once more: one row per act
	// (I6), never an amended one.
	req, _ = f.mustMove(f.transitionWrite(stored, domain.RequestStatusAccepted))
	if req.Rev != stored.Rev+1 {
		t.Errorf("second move rev = %d, want %d", req.Rev, stored.Rev+1)
	}
	if n := len(f.events(req.ID)); n != 2 {
		t.Errorf("history rows after two moves = %d, want 2", n)
	}
}

// TestApplyRequestTransitionQuestionIsOneWriteWithTheMove is I5's structural half:
// entering needs_information and appending its question are one transaction, the
// stamps 053 requires are set by the same statement, and a request that is asked
// something always has a question in its thread.
func TestApplyRequestTransitionQuestionIsOneWriteWithTheMove(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans001", domain.RequestStatusTriaging)

	w := f.transitionWrite(req, domain.RequestStatusNeedsInformation)
	w.Set = []string{"needs_info_since"}
	w.Comment = &RequestTransitionComment{
		Kind: domain.RequestCommentQuestion, Body: "Which workspace is the default for Berlin?",
		Author: "triager@transitions.test", AuthorUserID: w.ActorUserID,
	}
	got, comment := f.mustMove(w)
	if comment == nil {
		t.Fatal("a question move wrote no comment")
	}
	if comment.Kind != domain.RequestCommentQuestion || comment.Body != "Which workspace is the default for Berlin?" {
		t.Errorf("stored comment = %+v, want the question as written", comment)
	}
	thread := f.comments(req.ID)
	if len(thread) != 1 || thread[0].ID != comment.ID {
		t.Errorf("thread = %+v, want exactly the question", thread)
	}
	// 053 refuses a needs_information row with no needs_info_since, so a missing
	// stamp would have failed the write; asserting it proves the column is set
	// and not merely accepted.
	if got.NeedsInfoSince == nil {
		t.Error("needs_information stored no needs_info_since")
	}
	if got.State != domain.RequestStatusNeedsInformation {
		t.Errorf("state = %s, want needs_information", got.State)
	}
	// The history row names the comment it wrote, so the thread and the history
	// agree about which act asked it.
	events := f.events(req.ID)
	if len(events) != 1 {
		t.Fatalf("history rows = %d, want 1", len(events))
	}
	if want := fmt.Sprintf(`"comment_id":%d`, comment.ID); !strings.Contains(events[0].Detail, want) {
		t.Errorf("history detail %q does not name the question comment (%s)", events[0].Detail, want)
	}
}

// TestApplyRequestTransitionAnswerRestoresThePriorState is I5's other half, and
// the ticket's "answer restores prior state" criterion: the answer's destination
// is read from the request's own history, the answer comment lands in the same
// write, and the wait is NOT cleared (D8: a later clock still excludes it).
func TestApplyRequestTransitionAnswerRestoresThePriorState(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans002", domain.RequestStatusAccepted)

	asked := f.transitionWrite(req, domain.RequestStatusNeedsInformation)
	asked.Set = []string{"needs_info_since"}
	asked.Comment = &RequestTransitionComment{
		Kind: domain.RequestCommentQuestion, Body: "Do you need the existing accounts moved too?",
		Author: "triager@transitions.test", AuthorUserID: asked.ActorUserID,
	}
	waiting, question := f.mustMove(asked)

	prior, err := f.r.RequestPriorState(f.ctx, req.ID, f.org, req.TeamID)
	if err != nil {
		t.Fatalf("RequestPriorState: %v", err)
	}
	if prior != domain.RequestStatusAccepted {
		t.Fatalf("prior state = %q, want accepted (it comes from the history)", prior)
	}

	answer := f.transitionWrite(waiting, prior)
	answer.Comment = &RequestTransitionComment{
		Kind: domain.RequestCommentAnswer, Body: "No, just the new ones.",
		AnswerTo: question.ID, Author: "requester@transitions.test", AuthorUserID: answer.ActorUserID,
	}
	restored, reply := f.mustMove(answer)
	if restored.State != domain.RequestStatusAccepted {
		t.Errorf("state after the answer = %s, want accepted", restored.State)
	}
	if reply == nil || reply.Kind != domain.RequestCommentAnswer || reply.RequestID != req.ID {
		t.Fatalf("answer comment = %+v, want an answer on request %d", reply, req.ID)
	}
	if restored.NeedsInfoSince == nil {
		t.Error("the answer cleared needs_info_since: D8 keeps the wait so a clock can exclude it")
	}
	// The thread reads in order: the question, then its answer.
	thread := f.comments(req.ID)
	if len(thread) != 2 || thread[0].Kind != domain.RequestCommentQuestion || thread[1].Kind != domain.RequestCommentAnswer {
		t.Errorf("thread = %+v, want question then answer", thread)
	}
	// Historical names only: the answer's event detail names the question id it
	// answered.
	events := f.events(req.ID)
	if len(events) != 2 {
		t.Fatalf("history rows = %d, want 2", len(events))
	}
	if want := fmt.Sprintf(`"answer_to":%d`, question.ID); !strings.Contains(events[1].Detail, want) {
		t.Errorf("answer history detail %q does not name the question (%s)", events[1].Detail, want)
	}
}

// TestApplyRequestTransitionRefusesAnswersThatNameNoQuestion pins the answer's
// prerequisite in the write itself: an id that names nothing, another request's
// comment and a comment that is not a question are all refused, and each refusal
// writes nothing at all (no state move, no comment, no event).
func TestApplyRequestTransitionRefusesAnswersThatNameNoQuestion(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans003", domain.RequestStatusTriaging)
	other := f.newRequest("trans003b", domain.RequestStatusTriaging)

	// A question on the OTHER request, and a plain comment on this one.
	otherQ, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
		RequestID: other.ID, OrgID: f.org, TeamID: other.TeamID,
		Kind: domain.RequestCommentQuestion, Body: "Someone else's question?", Author: "triager",
	})
	if err != nil {
		t.Fatalf("append foreign question: %v", err)
	}
	notAQuestion, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
		RequestID: req.ID, OrgID: f.org, TeamID: req.TeamID,
		Kind: domain.RequestCommentDiscussion, Body: "A side remark.", Author: "triager",
	})
	if err != nil {
		t.Fatalf("append ordinary comment: %v", err)
	}

	// Put the request in needs_information through the real path, with its own
	// question, so the answer has somewhere to land.
	asked := f.transitionWrite(req, domain.RequestStatusNeedsInformation)
	asked.Set = []string{"needs_info_since"}
	asked.Comment = &RequestTransitionComment{
		Kind: domain.RequestCommentQuestion, Body: "Which group should own it?",
		Author: "triager",
	}
	waiting, question := f.mustMove(asked)

	cases := []struct {
		name     string
		answerTo int64
	}{
		{"an id that names nothing", 987654321},
		{"another request's question", otherQ.ID},
		{"a comment that is not a question", notAQuestion.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := f.transitionWrite(waiting, domain.RequestStatusTriaging)
			w.Comment = &RequestTransitionComment{
				Kind: domain.RequestCommentAnswer, Body: "The platform group.",
				AnswerTo: tc.answerTo, Author: "requester",
			}
			got, _, err := f.r.ApplyRequestTransition(f.ctx, w)
			if err == nil {
				t.Fatalf("answer to %d succeeded (%v), want a refusal", tc.answerTo, got)
			}
			if !errors.Is(err, ErrRequestAnswerTarget) {
				t.Errorf("refusal = %v, want ErrRequestAnswerTarget", err)
			}
			f.assertNothingMoved(waiting, 2, 1) // the question's comment + the side remark; the question's one event
		})
	}

	// The real question still works, which is what proves the refusals were about
	// their targets rather than about answers in general.
	w := f.transitionWrite(waiting, domain.RequestStatusTriaging)
	w.Comment = &RequestTransitionComment{
		Kind: domain.RequestCommentAnswer, Body: "The platform group.", AnswerTo: question.ID, Author: "requester",
	}
	if got, _ := f.mustMove(w); got.State != domain.RequestStatusTriaging {
		t.Errorf("state = %s, want triaging", got.State)
	}
}

// TestApplyRequestTransitionDeliveryNeedsAStoredLink is I2 as the write enforces
// it: the decision counts the links the service resolved, and the write counts
// the relations this transaction can see. Neither a claimed count nor an
// arbitrary reference is delivery evidence.
func TestApplyRequestTransitionDeliveryNeedsAStoredLink(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans004", domain.RequestStatusInProgress)

	w := f.transitionWrite(req, domain.RequestStatusDelivered)
	w.Set = []string{"delivered_at"}
	w.OutcomeBrief = "SSO is live for the three Berlin workspaces; nothing else changed."
	w.RejectReason = ""

	// No link and no explicit no-artifact record: refused, nothing written.
	got, _, err := f.r.ApplyRequestTransition(f.ctx, w)
	if err == nil {
		t.Fatalf("a delivery with no evidence succeeded (%v), want a refusal", got)
	}
	if !errors.Is(err, ErrRequestDeliveryUnEvidenced) {
		t.Fatalf("refusal = %v, want ErrRequestDeliveryUnEvidenced", err)
	}
	f.assertNothingMoved(req, 0, 0)

	// The explicit no-artifact record is the other half of I2 and is enough.
	noArtifact := w
	noArtifact.NoArtifact = true
	delivered, _ := f.mustMove(noArtifact)
	if delivered.State != domain.RequestStatusDelivered || delivered.DeliveredAt == nil {
		t.Fatalf("no-artifact delivery stored %s delivered_at=%v", delivered.State, delivered.DeliveredAt)
	}
	if delivered.OutcomeBrief != w.OutcomeBrief {
		t.Errorf("outcome_brief = %q, want the normalised brief", delivered.OutcomeBrief)
	}

	// A stored delivered_by link is the other way, on a second request.
	second := f.newRequest("trans004b", domain.RequestStatusInProgress)
	if _, _, err := f.r.LinkRequest(f.ctx, f.linkWrite(second, domain.RequestLinkDeliveredBy, domain.RequestTargetTask, f.task, f.org)); err != nil {
		t.Fatalf("attach delivered_by: %v", err)
	}
	linked := f.transitionWrite(second, domain.RequestStatusDelivered)
	linked.Set = []string{"delivered_at"}
	linked.OutcomeBrief = "The migration ran; the 14 legacy accounts were left alone."
	if _, _, err := f.r.ApplyRequestTransition(f.ctx, linked); err != nil {
		t.Fatalf("a delivery with a stored link: %v", err)
	}
}

// TestApplyRequestTransitionRefusesAStaleDecision is the third criterion's
// concurrency half: the write applies only to the row the decision was made on.
func TestApplyRequestTransitionRefusesAStaleDecision(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans005", domain.RequestStatusNew)

	// A decision read at rev 1 that is written after another move landed at rev 2.
	moved, _ := f.mustMove(f.transitionWrite(req, domain.RequestStatusTriaging))
	stale := f.transitionWrite(req, domain.RequestStatusRejected)
	stale.RejectReason = "not for this team"

	got, comment, err := f.r.ApplyRequestTransition(f.ctx, stale)
	if err == nil {
		t.Fatalf("a write at a stale revision succeeded (%v, %v)", got, comment)
	}
	var revConflict *RequestRevConflict
	if !errors.As(err, &revConflict) {
		t.Fatalf("refusal = %v, want *RequestRevConflict", err)
	}
	if !errors.Is(err, ErrConflict) {
		t.Error("a stale revision must match ErrConflict so a handler maps it once")
	}
	if revConflict.Current != moved.Rev {
		t.Errorf("conflict Current = %d, want %d", revConflict.Current, moved.Rev)
	}
	f.assertNothingMoved(moved, 0, 1)

	// A revision that is current but a state that has moved on is its own
	// refusal: applying it would move a state the policy never saw.
	wrongState := f.transitionWrite(req, domain.RequestStatusRejected) // From = new, ExpectRev = 1
	wrongState.ExpectRev = moved.Rev
	wrongState.RejectReason = "not for this team"
	got, comment, err = f.r.ApplyRequestTransition(f.ctx, wrongState)
	if err == nil {
		t.Fatalf("a write from a state the row left succeeded (%v, %v)", got, comment)
	}
	var stateConflict *RequestStateConflict
	if !errors.As(err, &stateConflict) {
		t.Fatalf("refusal = %v, want *RequestStateConflict", err)
	}
	if stateConflict.Current != domain.RequestStatusTriaging || stateConflict.Expected != domain.RequestStatusNew {
		t.Errorf("conflict = %+v, want new → triaging", stateConflict)
	}
	f.assertNothingMoved(moved, 0, 1)
}

// TestApplyRequestTransitionFailedEventRollsBack is the atomicity half of the
// first criterion: state, revision, columns and the comment are one transaction
// with the history row, so a failing event insert leaves NOTHING behind. The
// failure is injected on request_events for this test's own request id, so the
// real INSERT is the statement that fails.
func TestApplyRequestTransitionFailedEventRollsBack(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans006", domain.RequestStatusTriaging)

	w := f.transitionWrite(req, domain.RequestStatusNeedsInformation)
	w.Set = []string{"needs_info_since"}
	w.Comment = &RequestTransitionComment{
		Kind: domain.RequestCommentQuestion, Body: "Which group owns it?",
		Author: "triager",
	}

	fn := fmt.Sprintf("request_transitions_test_fail_event_%d", f.suffix)
	if _, err := f.r.db.Exec(f.ctx, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %[1]s() RETURNS trigger AS $$
		BEGIN
			IF NEW.action = 'transition' AND NEW.request_id = %[2]d THEN
				RAISE EXCEPTION 'injected failure while recording the transition event';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql`, fn, req.ID)); err != nil {
		t.Fatalf("create the failure injector: %v", err)
	}
	dropInjector := func() {
		t.Helper()
		if _, err := f.r.db.Exec(f.ctx, `DROP TRIGGER IF EXISTS request_transitions_test_fail_event ON request_events`); err != nil {
			t.Fatalf("drop the failure injector trigger: %v", err)
		}
		if _, err := f.r.db.Exec(f.ctx, `DROP FUNCTION IF EXISTS `+fn+`()`); err != nil {
			t.Fatalf("drop the failure injector function: %v", err)
		}
	}
	t.Cleanup(dropInjector)
	if _, err := f.r.db.Exec(f.ctx, `CREATE TRIGGER request_transitions_test_fail_event
		BEFORE INSERT ON request_events FOR EACH ROW EXECUTE FUNCTION `+fn+`()`); err != nil {
		t.Fatalf("install the failure injector: %v", err)
	}

	got, comment, err := f.r.ApplyRequestTransition(f.ctx, w)
	if err == nil {
		t.Fatalf("the injected event failure produced no error (row %v, comment %v)", got, comment)
	}
	if got != nil || comment != nil {
		t.Fatalf("a rolled-back transition returned row %v comment %v", got, comment)
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
		t.Errorf("an event failure read as %v; it is a server fault, not a client answer", err)
	}
	// Nothing moved, and no comment survived the rollback: a request can never sit
	// in needs_information with a question that "was not written".
	f.assertNothingMoved(req, 0, 0)

	// With the fault gone the same move at the same revision succeeds, which is
	// what proves the rollback left no stuck state behind.
	dropInjector()
	applied, question := f.mustMove(w)
	if applied.State != domain.RequestStatusNeedsInformation || question == nil {
		t.Fatalf("retry stored %s with comment %v", applied.State, question)
	}
	if n := len(f.events(req.ID)); n != 1 {
		t.Errorf("retry history rows = %d, want 1", n)
	}
}

// TestApplyRequestTransitionFirstWriteWinsTimestamps pins 053's first-write-wins
// rule as this write implements it: a second entry into a state does not move the
// stamp, reopened_count counts every reopen, and a reopen out of duplicate
// detaches the pointer and the live relation row together while the fold stays in
// the history.
func TestApplyRequestTransitionFirstWriteWinsTimestamps(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans007", domain.RequestStatusInProgress)

	deliver := f.transitionWrite(req, domain.RequestStatusDelivered)
	deliver.Set = []string{"delivered_at"}
	deliver.OutcomeBrief = "First delivery."
	deliver.NoArtifact = true
	first, _ := f.mustMove(deliver)
	if first.DeliveredAt == nil {
		t.Fatal("delivered stored no delivered_at")
	}
	firstDeliveredAt := *first.DeliveredAt

	// Reopen from delivered (D6's requester half): the reachable path to a second
	// delivery, where the first delivery's stamp must not move.
	reopen := f.transitionWrite(first, domain.RequestStatusTriaging)
	reopen.Set = []string{"reopened_at", "reopened_count"}
	reopened, _ := f.mustMove(reopen)
	if reopened.ReopenedCount != 1 || reopened.ReopenedAt == nil {
		t.Fatalf("reopen stored count %d at %v, want 1 and a time", reopened.ReopenedCount, reopened.ReopenedAt)
	}
	firstReopenedAt := *reopened.ReopenedAt

	// Back along the working chain to a second delivery.
	current := reopened
	for _, to := range []domain.RequestStatus{domain.RequestStatusAccepted, domain.RequestStatusInProgress} {
		current, _ = f.mustMove(f.transitionWrite(current, to))
	}
	again := f.transitionWrite(current, domain.RequestStatusDelivered)
	again.Set = []string{"delivered_at"}
	again.OutcomeBrief = "Second delivery."
	again.NoArtifact = true
	redelivered, _ := f.mustMove(again)
	if redelivered.DeliveredAt == nil || !redelivered.DeliveredAt.Equal(firstDeliveredAt) {
		t.Errorf("delivered_at moved from %v to %v: 053 makes it first-write-wins", firstDeliveredAt, redelivered.DeliveredAt)
	}

	// Fold, then reopen out of duplicate: the second reopen counts, the
	// reopened_at stamp does not move, the pointer and the live relation row leave
	// together, and the fold stays readable in the append-only history.
	canonical := f.newRequest("trans007b", domain.RequestStatusTriaging)
	// MarkDuplicate's caller supplies the actor (its helper does not default one
	// the way this file's write does), and the service always passes a label.
	fold := f.dupWrite(redelivered, canonical.ID)
	actorID := f.user
	fold.Actor, fold.ActorUserID = "triager@transitions.test", &actorID
	folded, changed, err := f.r.MarkDuplicate(f.ctx, fold)
	if err != nil || !changed {
		t.Fatalf("fold: err %v changed %v", err, changed)
	}
	detach := f.transitionWrite(folded, domain.RequestStatusTriaging)
	detach.Set = []string{"reopened_at", "reopened_count"}
	detach.ClearDuplicateOf = true
	withdrawn, _ := f.mustMove(detach)
	if withdrawn.DuplicateOf != nil {
		t.Errorf("a reopened duplicate still points at %d", *withdrawn.DuplicateOf)
	}
	if withdrawn.ReopenedCount != 2 {
		t.Errorf("reopened_count = %d, want 2", withdrawn.ReopenedCount)
	}
	if withdrawn.ReopenedAt == nil || !withdrawn.ReopenedAt.Equal(firstReopenedAt) {
		t.Errorf("reopened_at moved from %v to %v: 053 makes it first-write-wins", firstReopenedAt, withdrawn.ReopenedAt)
	}
	if links := f.linkRows(req.ID); len(links) != 0 {
		t.Errorf("the reopened duplicate still has %d link rows, want none", len(links))
	}
	detachEvents := 0
	for _, ev := range f.events(req.ID) {
		if ev.Action == domain.RequestActionTransition && ev.FromStatus == domain.RequestStatusDuplicate {
			detachEvents++
			if want := fmt.Sprintf(`"detached_duplicate_of":%d`, canonical.ID); !strings.Contains(ev.Detail, want) {
				t.Errorf("detach detail %q does not name the canonical (%s)", ev.Detail, want)
			}
		}
	}
	if detachEvents != 1 {
		t.Errorf("detach history rows = %d, want exactly one", detachEvents)
	}
}

// TestApplyRequestTransitionAcceptsTheEntryTimestamps proves every column the
// policy can name is one this write can set. It is the cross-package half of the
// guard the service's test states from the other side: a column the policy learns
// to emit that this table cannot write fails here.
func TestApplyRequestTransitionWritesEverySupportedColumn(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans008", domain.RequestStatusTriaging)

	// The order the policy names them in, through one reachable path: accepted_at
	// and closed_at on one request (accepted at rev 1, then rejected at rev 2),
	// plus the reopen pair and the two remaining stamps on a second.
	w := f.transitionWrite(req, domain.RequestStatusAccepted)
	w.Set = []string{"accepted_at"}
	actor := f.user
	w.AcceptedBy = &actor
	accepted, _ := f.mustMove(w)
	if accepted.AcceptedAt == nil || accepted.AcceptedBy == nil || *accepted.AcceptedBy != actor {
		t.Fatalf("acceptance stored accepted_at=%v accepted_by=%v, want both set to user %d",
			accepted.AcceptedAt, accepted.AcceptedBy, actor)
	}

	reject := f.transitionWrite(accepted, domain.RequestStatusRejected)
	reject.Set = []string{"closed_at"}
	reject.RejectReason = "  Duplicate of the workspace migration we already declined.  "
	rejected, _ := f.mustMove(reject)
	if rejected.ClosedAt == nil {
		t.Error("rejected stored no closed_at")
	}
	if rejected.RejectReason != "Duplicate of the workspace migration we already declined." {
		t.Errorf("reject_reason = %q, want it trimmed", rejected.RejectReason)
	}
	// closed_at is first-write-wins: a reopen then a second close must not move it.
	reopen := f.transitionWrite(rejected, domain.RequestStatusTriaging)
	reopen.Set = []string{"reopened_at", "reopened_count"}
	reopened, _ := f.mustMove(reopen)
	reclose := f.transitionWrite(reopened, domain.RequestStatusRejected)
	reclose.Set = []string{"closed_at"}
	reclose.RejectReason = "Declined again for the same reason."
	final, _ := f.mustMove(reclose)
	if final.ClosedAt == nil || !final.ClosedAt.Equal(*rejected.ClosedAt) {
		t.Errorf("closed_at moved from %v to %v: 053 makes it first-write-wins", rejected.ClosedAt, final.ClosedAt)
	}
}

// TestApplyRequestTransitionRefusesShapesThatWouldLie covers the shape checks:
// each of these is a caller bug the service would never emit, and each must fail
// loudly rather than write something the schema would accept and a reader would
// believe.
func TestApplyRequestTransitionRefusesShapesThatWouldLie(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans009", domain.RequestStatusAccepted)
	base := f.transitionWrite(req, domain.RequestStatusInProgress)

	cases := []struct {
		name   string
		mutate func(*RequestTransitionWrite)
		want   error
	}{
		{"no destination", func(w *RequestTransitionWrite) { w.To = "" }, ErrRequestTransitionShape},
		{"same state twice", func(w *RequestTransitionWrite) { w.To = domain.RequestStatusAccepted }, ErrRequestTransitionShape},
		{"a fold attempted here", func(w *RequestTransitionWrite) { w.To = domain.RequestStatusDuplicate }, ErrRequestTransitionShape},
		{"no expected revision", func(w *RequestTransitionWrite) { w.ExpectRev = 0 }, ErrRequestTransitionShape},
		{"a column that is not a transition's", func(w *RequestTransitionWrite) { w.Set = []string{"subject"} }, ErrRequestTransitionShape},
		{"a delivery with no brief", func(w *RequestTransitionWrite) {
			w.To = domain.RequestStatusDelivered
			w.OutcomeBrief = "   "
		}, ErrRequestTransitionShape},
		{"a reject with no reason", func(w *RequestTransitionWrite) {
			w.To = domain.RequestStatusRejected
			w.RejectReason = ""
		}, ErrRequestTransitionShape},
		{"an acceptance with no actor", func(w *RequestTransitionWrite) {
			w.To = domain.RequestStatusRequesterAccepted
			w.Set = []string{"accepted_at"}
			w.AcceptedBy = nil
		}, ErrRequestTransitionShape},
		{"a detach from a non-duplicate", func(w *RequestTransitionWrite) { w.ClearDuplicateOf = true }, ErrRequestTransitionShape},
		{"a question that is not entering needs_information", func(w *RequestTransitionWrite) {
			w.Comment = &RequestTransitionComment{Kind: domain.RequestCommentQuestion, Body: "why?", Author: "t"}
		}, ErrRequestTransitionShape},
		{"a question with no words", func(w *RequestTransitionWrite) {
			w.To = domain.RequestStatusNeedsInformation
			w.Set = []string{"needs_info_since"}
			w.Comment = &RequestTransitionComment{Kind: domain.RequestCommentQuestion, Body: " ", Author: "t"}
		}, ErrRequestTransitionShape},
		{"an answer that does not leave needs_information", func(w *RequestTransitionWrite) {
			w.Comment = &RequestTransitionComment{Kind: domain.RequestCommentAnswer, Body: "ok", AnswerTo: 4, Author: "t"}
		}, ErrRequestTransitionShape},
		{"an ordinary comment carried by a transition", func(w *RequestTransitionWrite) {
			w.Comment = &RequestTransitionComment{Kind: domain.RequestCommentDiscussion, Body: "hi", Author: "t"}
		}, ErrRequestTransitionShape},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := base
			tc.mutate(&w)
			got, comment, err := f.r.ApplyRequestTransition(f.ctx, w)
			if err == nil {
				t.Fatalf("the write succeeded (%v, %v), want a refusal", got, comment)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("refusal = %v, want %v", err, tc.want)
			}
			f.assertNothingMoved(req, 0, 0)
		})
	}
}

// TestApplyRequestTransitionWrongTenantIsNotFound proves a foreign tenant is the
// package's one not-found and writes nothing: the write is scoped by org AND
// team, so another team's request cannot be moved by naming its id.
func TestApplyRequestTransitionWrongTenantIsNotFound(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans010", domain.RequestStatusNew)

	w := f.transitionWrite(req, domain.RequestStatusTriaging)
	w.TeamID = f.team2 // the same org's other team
	got, comment, err := f.r.ApplyRequestTransition(f.ctx, w)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a cross-team write: err = %v (row %v comment %v), want ErrNotFound", err, got, comment)
	}
	f.assertNothingMoved(req, 0, 0)

	w = f.transitionWrite(req, domain.RequestStatusTriaging)
	w.OrgID = f.other
	if _, _, err := f.r.ApplyRequestTransition(f.ctx, w); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a cross-org write: err = %v, want ErrNotFound", err)
	}
	f.assertNothingMoved(req, 0, 0)
}

// TestRequestPriorStateReadsTheHistory pins where an answer's destination comes
// from: the newest transition INTO needs_information, tenant-scoped, and empty
// when the history does not say. 053 keeps no prior-state column on purpose, so a
// second source would be a second answer.
func TestRequestPriorStateReadsTheHistory(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans011", domain.RequestStatusInProgress)

	// No history at all: no answer and no error — the caller must not guess.
	prior, err := f.r.RequestPriorState(f.ctx, req.ID, f.org, req.TeamID)
	if err != nil {
		t.Fatalf("RequestPriorState with no history: %v", err)
	}
	if prior != "" {
		t.Fatalf("prior state = %q with no history, want empty", prior)
	}

	// Asked from in_progress, answered, asked again from accepted: the NEWEST
	// question is the one an answer is about, so its from_state wins.
	asked := f.transitionWrite(req, domain.RequestStatusNeedsInformation)
	asked.Set = []string{"needs_info_since"}
	asked.Comment = &RequestTransitionComment{Kind: domain.RequestCommentQuestion, Body: "Cert?", Author: "t"}
	waiting, question := f.mustMove(asked)
	answer := f.transitionWrite(waiting, domain.RequestStatusInProgress)
	answer.Comment = &RequestTransitionComment{Kind: domain.RequestCommentAnswer, Body: "Attached.", AnswerTo: question.ID, Author: "r"}
	answered, _ := f.mustMove(answer)

	askedAgain := f.transitionWrite(answered, domain.RequestStatusNeedsInformation)
	askedAgain.Set = []string{"needs_info_since"}
	askedAgain.Comment = &RequestTransitionComment{Kind: domain.RequestCommentQuestion, Body: "And the other cert?", Author: "t"}
	waitingAgain, _ := f.mustMove(askedAgain)

	prior, err = f.r.RequestPriorState(f.ctx, req.ID, f.org, req.TeamID)
	if err != nil {
		t.Fatalf("RequestPriorState: %v", err)
	}
	if prior != domain.RequestStatusInProgress {
		t.Errorf("prior state = %q, want in_progress (the state the newest question interrupted before the answer)", prior)
	}
	if waitingAgain.State != domain.RequestStatusNeedsInformation {
		t.Fatalf("state = %s, want needs_information", waitingAgain.State)
	}

	// Another team's tenant has no history of this request, so the answer is empty
	// (never another team's state), which the policy refuses rather than acting on.
	if prior, err := f.r.RequestPriorState(f.ctx, req.ID, f.org, f.team2); err != nil || prior != "" {
		t.Errorf("a cross-team prior-state read = (%q, %v), want empty and no error", prior, err)
	}
}

// TestRequestLatestQuestion pins the lookup the answer path uses when the
// requester did not name a question: the newest question ON this request (never
// another request's), and nil when the thread has none.
func TestRequestLatestQuestion(t *testing.T) {
	f := newRequestTransitionsFixture(t)
	req := f.newRequest("trans012", domain.RequestStatusTriaging)
	other := f.newRequest("trans012b", domain.RequestStatusTriaging)

	none, err := f.r.RequestLatestQuestion(f.ctx, req.ID, f.org, req.TeamID)
	if err != nil {
		t.Fatalf("RequestLatestQuestion with no thread: %v", err)
	}
	if none != nil {
		t.Fatalf("latest question = %+v on an empty thread, want nil", none)
	}

	if _, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
		RequestID: other.ID, OrgID: f.org, TeamID: other.TeamID,
		Kind: domain.RequestCommentQuestion, Body: "Not this request's question.", Author: "t",
	}); err != nil {
		t.Fatalf("append foreign question: %v", err)
	}
	first, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
		RequestID: req.ID, OrgID: f.org, TeamID: req.TeamID,
		Kind: domain.RequestCommentQuestion, Body: "First question.", Author: "t",
	})
	if err != nil {
		t.Fatalf("append first question: %v", err)
	}
	if _, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
		RequestID: req.ID, OrgID: f.org, TeamID: req.TeamID,
		Kind: domain.RequestCommentDiscussion, Body: "A remark, not a question.", Author: "t",
	}); err != nil {
		t.Fatalf("append remark: %v", err)
	}
	second, err := f.r.AppendRequestComment(f.ctx, AppendRequestCommentRequest{
		RequestID: req.ID, OrgID: f.org, TeamID: req.TeamID,
		Kind: domain.RequestCommentQuestion, Body: "Second question.", Author: "t",
	})
	if err != nil {
		t.Fatalf("append second question: %v", err)
	}

	latest, err := f.r.RequestLatestQuestion(f.ctx, req.ID, f.org, req.TeamID)
	if err != nil {
		t.Fatalf("RequestLatestQuestion: %v", err)
	}
	if latest == nil || latest.ID != second.ID {
		t.Fatalf("latest question = %+v, want %d (the newest question on THIS request)", latest, second.ID)
	}
	if latest.ID == first.ID {
		t.Error("the lookup answered the older question: an answer must reply to the one that is open")
	}
	if other, err := f.r.RequestLatestQuestion(f.ctx, req.ID, f.org, f.team2); err != nil || other != nil {
		t.Errorf("a cross-team lookup = (%+v, %v), want none and no error", other, err)
	}
}
