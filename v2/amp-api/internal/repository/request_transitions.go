package repository

// Team requests: the one atomic write behind a lifecycle move (epic 202, story
// 548, AMP #3960; approved product contract artifact 36 v2 §1, §2 and §4; the
// pure decision is service/request_transitions.go, AMP #3953; build slices S5
// and S6 of discoveries/team-inbox-approved-build-slices.md).
//
// ApplyRequestTransition is the write half of the request state machine. It
// takes a plan the policy has already approved — the destination, the columns
// the same write must set, the question or answer comment, the actor an
// acceptance records — and performs exactly it, once, in one transaction:
//
//	lock the request row (SELECT … FOR UPDATE)
//	  → refuse unless the row is still in the state and at the revision the
//	    decision was made on
//	  → for an answer, refuse unless answer_to names a question comment of this
//	    request (I5: an answer answers something)
//	  → for a delivery with no explicit no-artifact record, refuse unless the
//	    request has a stored delivered_by link (I2: evidence is a relation, not
//	    a count the caller reports)
//	  → UPDATE state, the plan's columns, the plan's content, rev + 1, updated_at
//	  → append the question or answer comment, in the same transaction
//	  → detach from a canonical, when the move is the one reopen that leaves
//	    duplicate (D7): duplicate_of and the live duplicate_of link row go
//	  → append exactly ONE request_events row (I6)
//	commit — or nothing at all.
//
// Three properties this layer is responsible for, and how each is kept:
//
//  1. The decision and the write describe the same row. The row is locked, and
//     the write refuses unless its state still equals the plan's From AND its
//     rev still equals the caller's ExpectRev. Both are required: rev alone is
//     the compare-and-set every other request write uses, and the From check is
//     what turns "somebody else moved it" into a refusal that says so instead
//     of a write that silently applies a move to a state the policy never
//     approved. Every state-changing write in this package bumps rev
//     (MarkDuplicate, DetachDuplicate, claim/release/assign, this file), so no
//     concurrent move can slip between the caller's read and this lock.
//
//  2. Nothing is half-written. State, revision, the state-entry timestamps, the
//     question/answer comment, the duplicate detach and the history row are one
//     statement-set in one transaction. A failure anywhere rolls all of it back:
//     there is no window in which a request sits in needs_information with no
//     question in its thread (I5), or has moved with no history row (I6), or has
//     been reopened with its canonical pointer still set (053 refuses that
//     instant outright).
//
//  3. The comment is the evidence. Entering needs_information REQUIRES the
//     question, and the comment row is inserted here rather than left to a
//     second call: a caller cannot move the request first and append the
//     question later, because there is no such sequence. The answer's side is
//     symmetric — answer_to must name a real question comment of this request,
//     read in this transaction, so a positive integer that names nothing (or
//     another request's comment) is refused rather than stored.
//
// What this file deliberately does NOT do:
//
//   - It decides no move. Every legality, capability and evidence question is
//     service/request_transitions.go's (AMP #3953); this file would happily
//     apply a plan the policy would never produce, which is why it is not
//     exported to any surface and its structural checks are about shape (does
//     the state carry what 053 requires? is that column one the plan can name?),
//     never about who may do what.
//   - It does not fold a duplicate. `To == duplicate` is refused here: a fold
//     writes the duplicate_of relation row as well as the state
//     (MarkDuplicate, request_links.go), and doing that in two places would be
//     two answers to "what is a canonical?". The service routes a duplicate
//     plan to MarkDuplicate, which shares this file's concurrency contract
//     (ExpectRev, the row lock, one transition event).
//   - It does not audit. The audit row is the service's (service/audit.go), and
//     it is written after the commit because the change already happened.
//
// Every read and write goes through request_rows.go's contract (requestSelect,
// requestTenantWhere, requestCols, scanRequest) and does not edit it. The tenant
// is required on the write and re-stated in the UPDATE, so a call naming another
// team's ids writes nothing and is answered exactly like an id that does not
// exist (ErrNotFound, the request surface's one 404).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// ErrRequestDeliveryUnEvidenced reports a delivery refused because the request
// has no stored delivered_by link and the caller did not record the explicit
// no-artifact case. It exists because I2's evidence is a relation, not a number:
// the policy decides from a count the service resolved a moment earlier, and the
// count is not the relation — a link can be detached between the two. This
// write refuses to store a delivery the request's own lineage does not evidence,
// so a delivery that says "it exists" always has a row that says where.
var ErrRequestDeliveryUnEvidenced = errors.New("a delivery is evidenced by a delivered_by link (a task, an artifact or a receipt) or by the explicit no-artifact record")

// ErrRequestAnswerTarget reports an answer whose answer_to does not name a
// question comment on the same request. I5 makes the pair the record: an answer
// with no question is not an answer, and answering someone else's question (or
// a comment that is not a question) would put the thread out of order.
var ErrRequestAnswerTarget = errors.New("answer_to must name a question comment on the same request")

// ErrRequestTransitionShape marks a lifecycle write whose own shape is wrong: a
// missing end, the same state twice, an unknown or impossible column, a delivery
// with no brief, an acceptance with no actor, a fold attempted through this
// path. Like ErrRequestClaimant it reports a caller bug — the caller is the
// service, and the policy already refused everything a person could ask for
// wrongly — rather than a refusal worth showing anyone.
var ErrRequestTransitionShape = errors.New("a lifecycle write carries the plan it applies: both ends, the columns for them, and their evidence")

// RequestStateConflict reports a lifecycle write refused because the request is
// no longer in the state the caller's decision was made on. Current is the
// state the row is actually in, so a caller can say so (and tell a client to
// reload) without a second read, and nothing was written.
//
// It matches ErrConflict for the same reason RequestClaimHeld does: a handler
// that only distinguishes 409 from success needs no special case.
type RequestStateConflict struct {
	RequestID int
	Expected  domain.RequestStatus
	Current   domain.RequestStatus
}

func (e *RequestStateConflict) Error() string {
	return fmt.Sprintf("request %d is now %s, not %s: it moved since the transition was decided, so reload it and decide again", e.RequestID, e.Current, e.Expected)
}

// Is makes errors.Is(err, ErrConflict) true for a state that moved.
func (e *RequestStateConflict) Is(target error) bool { return target == ErrConflict }

// RequestTransitionComment is the discussion row a move carries, when it carries
// one: the team's question (I5's prerequisite for needs_information) or the
// requester's answer (what returns the request to where the question found it).
// It is inserted in the same transaction as the state move, so neither can exist
// without the other.
//
// Author and AuthorUserID are the caller's to supply — the service resolves the
// principal into a display snapshot plus a nullable AMP user — so nothing in the
// question's or answer's text decides who said it.
type RequestTransitionComment struct {
	// Kind is question or answer; no other kind is carried by a transition.
	Kind domain.RequestCommentKind
	// Body is the words. Required, non-blank, bounded like a brief (the domain
	// validates the bound; 054's CHECK is the generous backstop).
	Body string
	// Author is the display name captured at write time, and AuthorUserID the
	// AMP user behind it when there is one (a connector or service client posts
	// as its own integration identity, with no AMP user).
	Author       string
	AuthorUserID *int
	// AnswerTo names the question comment an answer replies to. It is 0 for a
	// question and is verified against this request's thread.
	AnswerTo int64
}

// RequestTransitionWrite is the caller's part of one lifecycle move: the plan,
// spelled as the columns and values this write must carry. It is a translation
// of service.RequestTransitionPlan, not a second policy — every field is read
// from the plan (the destination, Set, the question, the answer's target, the
// acceptance's actor) or from the caller's authenticated identity.
type RequestTransitionWrite struct {
	// The request and its whole tenant. Both halves of the tenant are required:
	// a write scoped by only one of them would be a cross-team (or cross-org)
	// write primitive.
	RequestID int
	OrgID     int
	TeamID    int

	// From and To are the move's ends. From is the state the decision was made
	// on and is re-checked under the row lock; a row that has moved on is
	// RequestStateConflict and writes nothing.
	From domain.RequestStatus
	To   domain.RequestStatus

	// ExpectRev is the revision the decision was made on, and is REQUIRED
	// (rev starts at 1). The service passes the revision it read — either the
	// client's If-Match or the row it loaded — so two decisions on the same
	// read can never both land: the second is RequestRevConflict.
	ExpectRev int

	// Set names the further columns this write must set, by their schema column
	// names — exactly the plan's RequestTransitionPlan.Set. The names are the
	// same words the policy uses (they are the columns' own names), and an
	// unknown name is refused rather than ignored: an ignored column would read
	// to the caller as one that was written. The supported set is
	// RequestTransitionColumnSupported.
	Set []string

	// OutcomeBrief is the delivery note (with To == delivered), and RejectReason
	// why the request is declined (with To == rejected). Both are normalized by
	// the domain before they reach here.
	OutcomeBrief string
	RejectReason string

	// NoArtifact is the explicit "the outcome exists and there is nothing to
	// link" record I2 allows in place of a delivered_by link. It is an explicit
	// flag, not the absence of one: with it unset, a delivery needs the link.
	NoArtifact bool

	// AcceptedBy is the user recorded in accepted_by for a requester's
	// acceptance. The policy fills it from the actor under the sign-in
	// conditions it re-checks; this layer never derives it.
	AcceptedBy *int

	// ClearDuplicateOf detaches the request from its canonical in the same
	// write, for the one reopen that leaves duplicate (D7). 053 refuses a
	// duplicate with no canonical, so the state move and the detach cannot be
	// split even for an instant.
	ClearDuplicateOf bool

	// Comment is the question or answer appended with the move, when the plan
	// names one. Nil means the act carries no discussion row (an ordinary
	// triage step, a reject, a reopen).
	Comment *RequestTransitionComment

	// Actor is who the move is attributed to in the request's history, and
	// ActorUserID the AMP user behind that name. A blank actor is recorded as
	// "system" (054: history has no anonymous rows).
	Actor       string
	ActorUserID *int
}

// requestTransitionColumns lists the columns a lifecycle write understands, in
// the order the policy names them (RequestSetNeedsInfoSince … RequestSetReopenedCount),
// together with the expression that sets each. The expressions are the whole
// meaning of the column, and there are two kinds:
//
//   - the entry timestamps are first-write-wins (COALESCE), because 053 makes
//     them record when the request FIRST entered the state: a reopen and a
//     second delivery do not move them, and reopened_count is what says how
//     often. The database supplies the instant (NOW()), so the write cannot be
//     given a timestamp from outside and every entry time is the moment of the
//     act;
//   - reopened_count counts (+1).
//
// A column the plan can name and this table cannot is a bug, not a refusal, so
// an unknown name fails the write instead of being skipped.
var requestTransitionColumns = []struct {
	name string
	set  string
}{
	{"needs_info_since", "needs_info_since = COALESCE(needs_info_since, NOW())"},
	{"delivered_at", "delivered_at = COALESCE(delivered_at, NOW())"},
	{"accepted_at", "accepted_at = COALESCE(accepted_at, NOW())"},
	{"closed_at", "closed_at = COALESCE(closed_at, NOW())"},
	{"reopened_at", "reopened_at = COALESCE(reopened_at, NOW())"},
	{"reopened_count", "reopened_count = reopened_count + 1"},
}

// RequestTransitionColumnSupported reports whether name is a column a lifecycle
// write can set. The service's cross-package test walks its own RequestSet*
// constants through this, so a column the policy learns to emit without this
// table learning to write fails there rather than at a user's request.
func RequestTransitionColumnSupported(name string) bool {
	_, ok := requestTransitionColumn(name)
	return ok
}

func requestTransitionColumn(name string) (string, bool) {
	for _, c := range requestTransitionColumns {
		if c.name == name {
			return c.set, true
		}
	}
	return "", false
}

// ApplyRequestTransition applies one lifecycle move and returns the stored row
// plus the comment it appended, when it appended one.
//
// It succeeds only when the row is still what the decision was made on: same
// state, same revision, the question/answer and delivery evidence present, and a
// tenant that names the row. A refusal writes nothing at all — no rev bump, no
// comment, no history row.
func (r *Repo) ApplyRequestTransition(ctx context.Context, w RequestTransitionWrite) (*domain.TeamRequest, *domain.RequestComment, error) {
	if err := validateRequestTransitionWrite(w); err != nil {
		return nil, nil, err
	}
	if w.Actor = strings.TrimSpace(w.Actor); w.Actor == "" {
		// History has no anonymous rows (054's actor CHECK). An act whose caller
		// passed no identity is recorded as the system's; the service passes the
		// principal behind it.
		w.Actor = "system"
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("transition request %d: %w", w.RequestID, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	req, err := lockRequest(ctx, tx, RequestClaimWrite{RequestID: w.RequestID, OrgID: w.OrgID, TeamID: w.TeamID})
	if err != nil {
		return nil, nil, err
	}
	// The revision first, then the state. rev is the concurrency token every
	// request write shares, so a caller whose copy is stale is told the revision
	// it must reload — the same answer UpdateRequest and the claim writes give. A
	// caller that read the row at this revision but decided from a different state
	// is the other refusal, and saying which state the row is actually in is what
	// lets it re-decide rather than retry blindly.
	if err := checkRequestExpectRev(req, w.ExpectRev); err != nil {
		return nil, nil, err
	}
	if req.State != w.From {
		return nil, nil, &RequestStateConflict{RequestID: req.ID, Expected: w.From, Current: req.State}
	}
	// The answer's prerequisite, read in this transaction: the question it
	// replies to must be this request's own question comment. This is also the
	// point where a caller-supplied id that names nothing becomes an ordinary
	// refusal rather than a comment row pointing at air.
	if w.Comment != nil && w.Comment.Kind == domain.RequestCommentAnswer {
		question, err := getRequestComment(ctx, tx, w.Comment.AnswerTo, req.ID, req.OrgID, req.TeamID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, nil, fmt.Errorf("answer to comment %d on request %d: %w", w.Comment.AnswerTo, req.ID, ErrRequestAnswerTarget)
			}
			return nil, nil, err
		}
		if question.Kind != domain.RequestCommentQuestion {
			return nil, nil, fmt.Errorf("answer to comment %d on request %d: that comment is a %s, not a question: %w",
				w.Comment.AnswerTo, req.ID, question.Kind, ErrRequestAnswerTarget)
		}
	}
	// I2's other half. The policy decided from a count the service resolved
	// before this call; the relation is what this transaction can see, and the
	// row lock it holds is the same one every link write takes, so the count
	// below cannot change under it. A delivery with an explicit no-artifact
	// record skips the check because that record IS the evidence.
	if w.To == domain.RequestStatusDelivered && !w.NoArtifact {
		n, err := deliverLinkCount(ctx, tx, req.ID, req.OrgID)
		if err != nil {
			return nil, nil, err
		}
		if n == 0 {
			return nil, nil, fmt.Errorf("request %d: %w", req.ID, ErrRequestDeliveryUnEvidenced)
		}
	}

	// What the reopen out of duplicate detaches, read before the update
	// overwrites it: the link row to remove and the history detail that says
	// what the detach left behind.
	var detached *int
	if w.ClearDuplicateOf && req.DuplicateOf != nil {
		foldedInto := *req.DuplicateOf
		detached = &foldedInto
	}

	updated, err := applyRequestTransitionUpdate(ctx, tx, req, w)
	if err != nil {
		return nil, nil, err
	}
	if detached != nil {
		if err := removeDuplicateLink(ctx, tx, req.ID, *detached); err != nil {
			return nil, nil, err
		}
	}
	var comment *domain.RequestComment
	if w.Comment != nil {
		comment, err = insertRequestTransitionComment(ctx, tx, updated, *w.Comment)
		if err != nil {
			return nil, nil, err
		}
	}
	if err := insertRequestTransitionEvent(ctx, tx, updated, w, comment, detached); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("transition request %d: %w", w.RequestID, err)
	}
	return updated, comment, nil
}

// RequestPriorState answers where the request was before it entered
// needs_information, from its own transition history: the from_state of the
// newest transition whose to_state is needs_information.
//
// It exists because the schema keeps no prior-state column on purpose (053) — a
// second column could disagree with the history — and I5's answer restores the
// state the question interrupted. An empty answer means the history does not say
// (a request hand-edited into needs_information, or one whose question happened
// before history was recorded); the policy refuses that with its own message
// rather than guessing a destination.
//
// The read is scoped by the request's whole tenant, so another team's history is
// not reachable even with an id; an id in the wrong team simply has no history in
// this tenant, which answers empty (the policy refuses that) rather than
// distinguishing "not there" from "not yours". It is a read, not a lock: the answer's write
// re-checks the state it restores under its own row lock.
func (r *Repo) RequestPriorState(ctx context.Context, requestID, orgID, teamID int) (domain.RequestStatus, error) {
	if err := checkRequestWriteScope(requestID, orgID, teamID); err != nil {
		return "", err
	}
	var from string
	err := r.db.QueryRow(ctx, `SELECT from_state FROM request_events
		WHERE request_id = $1 AND org_id = $2 AND team_id = $3
		  AND action = $4 AND to_state = $5
		ORDER BY id DESC LIMIT 1`,
		requestID, orgID, teamID,
		string(domain.RequestActionTransition), string(domain.RequestStatusNeedsInformation),
	).Scan(&from)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("prior state of request %d: %w", requestID, err)
	}
	st, err := domain.NormalizeRequestStatus(from)
	if err != nil {
		return "", nil
	}
	return st, nil
}

// RequestLatestQuestion returns the newest question comment on the request, or
// (nil, nil) when the thread has none.
//
// It answers "what is the requester answering?" for the answer path when the
// caller did not name a question: I5 makes the question/answer pair the record,
// and the open question is the newest one. Reading it here (rather than letting a
// caller pass any positive integer) is what keeps answer_to in the thread; the
// write verifies the id it is given either way, in its own transaction.
//
// The read is tenant-scoped by a join on the request, so another team's thread is
// not reachable with an id (it answers "no question", which the write then refuses
// rather than acting on), and it uses the partial index 054 ships for exactly this
// query (request_id, id DESC WHERE kind = 'question').
func (r *Repo) RequestLatestQuestion(ctx context.Context, requestID, orgID, teamID int) (*domain.RequestComment, error) {
	if err := checkRequestWriteScope(requestID, orgID, teamID); err != nil {
		return nil, err
	}
	query := `SELECT ` + requestCommentCols("c") + `
		  FROM request_comments c
		  JOIN team_requests r ON r.id = c.request_id
		 WHERE c.request_id = $1 AND r.org_id = $2 AND r.team_id = $3 AND c.kind = $4
		 ORDER BY c.id DESC
		 LIMIT 1`
	c := &domain.RequestComment{}
	err := scanRequestComment(r.db.QueryRow(ctx, query, requestID, orgID, teamID, string(domain.RequestCommentQuestion)), c)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest question on request %d: %w", requestID, err)
	}
	return c, nil
}

// ---- The transaction body ----

// validateRequestTransitionWrite refuses a write whose own shape is wrong,
// before anything reaches the database. It checks structure only: whether the
// move is legal, who may make it and what evidence it needs are the policy's
// (service/request_transitions.go), and the state's own obligations (brief,
// reason, actor, canonical) are 053's CHECKs. Restating the three that would
// otherwise surface as a constraint violation is worth it here because each
// names a different missing piece.
func validateRequestTransitionWrite(w RequestTransitionWrite) error {
	if err := checkRequestWriteScope(w.RequestID, w.OrgID, w.TeamID); err != nil {
		return err
	}
	if w.From == "" || w.To == "" {
		return fmt.Errorf("request %d: a transition names the state it leaves and the state it enters: %w", w.RequestID, ErrRequestTransitionShape)
	}
	if w.From == w.To {
		return fmt.Errorf("request %d is already %s: %w", w.RequestID, w.From, ErrRequestTransitionShape)
	}
	if w.To == domain.RequestStatusDuplicate {
		// MarkDuplicate owns a fold: it writes the duplicate_of relation row and
		// flattens the chain as well as the state, and two places folding would be
		// two answers to "what is a canonical?".
		return fmt.Errorf("request %d cannot be folded through ApplyRequestTransition: a duplicate is MarkDuplicate's write (it owns the relation row): %w", w.RequestID, ErrRequestTransitionShape)
	}
	if w.ExpectRev < 1 {
		return fmt.Errorf("request %d: a lifecycle write carries the revision it was decided on (got %d): %w", w.RequestID, w.ExpectRev, ErrRequestTransitionShape)
	}
	for _, name := range w.Set {
		if !RequestTransitionColumnSupported(name) {
			return fmt.Errorf("request %d: %q is not a column a transition sets: %w", w.RequestID, name, ErrRequestTransitionShape)
		}
	}
	switch w.To {
	case domain.RequestStatusDelivered:
		if strings.TrimSpace(w.OutcomeBrief) == "" {
			return fmt.Errorf("request %d: delivered carries its outcome brief: %w", w.RequestID, ErrRequestTransitionShape)
		}
	case domain.RequestStatusRejected:
		if strings.TrimSpace(w.RejectReason) == "" {
			return fmt.Errorf("request %d: a rejected request carries its reject_reason: %w", w.RequestID, ErrRequestTransitionShape)
		}
	case domain.RequestStatusRequesterAccepted:
		if w.AcceptedBy == nil {
			return fmt.Errorf("request %d: an acceptance records the requester it belongs to in accepted_by: %w", w.RequestID, ErrRequestTransitionShape)
		}
	}
	if w.ClearDuplicateOf && w.From != domain.RequestStatusDuplicate {
		return fmt.Errorf("request %d: only a reopen out of duplicate detaches it, and the request is in %s: %w", w.RequestID, w.From, ErrRequestTransitionShape)
	}
	if c := w.Comment; c != nil {
		switch c.Kind {
		case domain.RequestCommentQuestion:
			if w.To != domain.RequestStatusNeedsInformation {
				return fmt.Errorf("request %d: a question is what entering needs_information carries, and this moves to %s: %w", w.RequestID, w.To, ErrRequestTransitionShape)
			}
			if strings.TrimSpace(c.Body) == "" || c.AnswerTo != 0 {
				return fmt.Errorf("request %d: a question has words and answers nothing: %w", w.RequestID, ErrRequestTransitionShape)
			}
		case domain.RequestCommentAnswer:
			if w.From != domain.RequestStatusNeedsInformation || c.AnswerTo <= 0 {
				return fmt.Errorf("request %d: an answer leaves needs_information and names the question it answers: %w", w.RequestID, ErrRequestTransitionShape)
			}
			if strings.TrimSpace(c.Body) == "" {
				return fmt.Errorf("request %d: an answer says something: %w", w.RequestID, ErrRequestTransitionShape)
			}
		default:
			return fmt.Errorf("request %d: a transition carries a question or an answer, not a %s comment: %w", w.RequestID, c.Kind, ErrRequestTransitionShape)
		}
	}
	return nil
}

// applyRequestTransitionUpdate writes the state, the plan's columns and the
// plan's content in one statement and reads the stored row back through the
// shared scanner.
//
// The SET clause is built in requestTransitionColumns' fixed order (not the
// caller's), so the statement is the same for the same plan and a test can
// compare it; rev + 1 and updated_at are added here, once, so every lifecycle
// write moves the concurrency token the same way and no caller can forget it.
// The WHERE clause repeats the tenant and the expected revision even though the
// row is locked: the lock proves the read was consistent, and the predicate
// proves the write is the one that was decided on.
func applyRequestTransitionUpdate(ctx context.Context, tx pgx.Tx, req *domain.TeamRequest, w RequestTransitionWrite) (*domain.TeamRequest, error) {
	// $1 is the request id and $2 the destination; the plan's columns follow in
	// requestTransitionColumns' order, then the content the destination carries.
	args := []any{req.ID, string(w.To)}
	set := []string{"state = $2"}
	add := func(expr string, value any) {
		args = append(args, value)
		set = append(set, fmt.Sprintf(expr, len(args)))
	}
	wanted := make(map[string]bool, len(w.Set))
	for _, name := range w.Set {
		wanted[name] = true
	}
	for _, column := range requestTransitionColumns {
		if wanted[column.name] {
			set = append(set, column.set)
		}
	}
	switch w.To {
	case domain.RequestStatusDelivered:
		// Trimmed here as well as in the domain: the column is what a requester
		// reads, and surrounding whitespace is not part of the sentence.
		add("outcome_brief = $%d", strings.TrimSpace(w.OutcomeBrief))
	case domain.RequestStatusRejected:
		add("reject_reason = $%d", strings.TrimSpace(w.RejectReason))
	}
	if wanted["accepted_at"] && w.AcceptedBy != nil {
		// accepted_by moves with accepted_at and is first-write-wins for the same
		// reason: a second acceptance must not re-attribute the first one's moment.
		add("accepted_by = COALESCE(accepted_by, $%d)", *w.AcceptedBy)
	}
	if w.ClearDuplicateOf {
		set = append(set, "duplicate_of = NULL")
	}
	n := len(args)
	sql := `UPDATE team_requests SET ` + strings.Join(set, ", ") + `, rev = rev + 1, updated_at = NOW()` +
		fmt.Sprintf(` WHERE id = $1 AND org_id = $%d AND team_id = $%d AND rev = $%d`, n+1, n+2, n+3) +
		` RETURNING ` + requestCols
	args = append(args, w.OrgID, w.TeamID, w.ExpectRev)
	updated := &domain.TeamRequest{}
	if err := scanRequest(tx.QueryRow(ctx, sql, args...), updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The row was locked a statement ago in this transaction, so a vanished
			// row means the tenant, the id or the revision did not match after all.
			// Answer it as the read path would for a row this tenant cannot name,
			// and as a conflict for a revision that moved under a deferred writer.
			if req.Rev != w.ExpectRev {
				return nil, &RequestRevConflict{RequestID: req.ID, Expected: w.ExpectRev, Current: req.Rev}
			}
			return nil, fmt.Errorf("request %d: %w", req.ID, ErrNotFound)
		}
		return nil, fmt.Errorf("transition request %d: %w", req.ID, err)
	}
	return updated, nil
}

// deliverLinkCount counts the request's delivered_by links, in the caller's
// transaction and therefore under the request's row lock. delivered_by is the
// one link kind 053/055 give delivery (D10: tasks, artifacts and receipts), and
// every such row was written by LinkRequest, which verified its target exists in
// this org before storing it — so the count is a count of verified relations,
// never of URLs or ids a caller reported.
//
// A link whose target is later deleted stays (055's tombstone rule: the relation
// is the record), and it still counts: the team did deliver by that, and the
// target's later deletion does not un-deliver the act.
func deliverLinkCount(ctx context.Context, tx pgx.Tx, requestID, orgID int) (int, error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM request_links
		WHERE request_id = $1 AND org_id = $2 AND link_kind = $3`,
		requestID, orgID, string(domain.RequestLinkDeliveredBy)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count delivered_by links of request %d: %w", requestID, err)
	}
	return n, nil
}

// removeDuplicateLink drops the live duplicate_of relation of a reopened
// duplicate, the same row DetachDuplicate removes: the pointer and the relation
// must stop being true together, while the fold itself stays readable in the
// request's history (request_events is append-only), which is what "don't delete
// lineage" means here.
func removeDuplicateLink(ctx context.Context, tx pgx.Tx, requestID, canonicalID int) error {
	_, err := tx.Exec(ctx, `DELETE FROM request_links
		WHERE request_id = $1 AND link_kind = $2 AND target_kind = $3 AND target_id = $4`,
		requestID, string(domain.RequestLinkDuplicateOf), string(domain.RequestTargetRequest), canonicalID)
	if err != nil {
		return fmt.Errorf("remove duplicate link of request %d: %w", requestID, err)
	}
	return nil
}

// insertRequestTransitionComment appends the move's question or answer, in the
// move's transaction, and returns the row as stored. It validates through the
// domain, so this cannot store a kind or a body the domain would refuse, and it
// requires an author for the same reason AppendRequestComment does: an
// unattributed row in a requester-visible thread is a bug, not a state.
func insertRequestTransitionComment(ctx context.Context, tx pgx.Tx, req *domain.TeamRequest, c RequestTransitionComment) (*domain.RequestComment, error) {
	stored := &domain.RequestComment{
		RequestID:    req.ID,
		Kind:         c.Kind,
		Body:         c.Body,
		Author:       strings.TrimSpace(c.Author),
		AuthorUserID: c.AuthorUserID,
	}
	if err := stored.Validate(); err != nil {
		return nil, fmt.Errorf("comment on request %d: %w", req.ID, err)
	}
	if stored.Author == "" {
		return nil, fmt.Errorf("comment on request %d: author is required: a comment says who said it", req.ID)
	}
	err := scanRequestComment(tx.QueryRow(ctx,
		`INSERT INTO request_comments (request_id, kind, body, author, author_user_id)
		 VALUES ($1, $2, $3, $4, $5) RETURNING `+requestCommentCols(""),
		req.ID, string(stored.Kind), stored.Body, stored.Author, stored.AuthorUserID), stored)
	if err != nil {
		return nil, fmt.Errorf("append comment on request %d: %w", req.ID, err)
	}
	return stored, nil
}

// transitionEventDetail is what one transition history row explains beyond its
// two states: which comment the move appended, and — for a reopen out of
// duplicate — which canonical it stopped pointing at. Ids only, never a question,
// an answer or a reason: a request's history is requester-visible and copying
// content into it would need its own redaction rule (the claim file's rule).
type transitionEventDetail struct {
	CommentID           int64 `json:"comment_id,omitempty"`
	AnswerTo            int64 `json:"answer_to,omitempty"`
	DetachedDuplicateOf *int  `json:"detached_duplicate_of,omitempty"`
}

// insertRequestTransitionEvent appends the one history row the move gets (I6), in
// the transaction that made the change, and supports the one shape a transition
// has: both states named (054's request_events_transition_states rule), which
// domain.RequestEvent.Validate enforces before the database would.
//
// It is its own insert rather than a call into request_claims.go's
// insertRequestEvent because that helper's input is a claim write, and into
// request_links.go's appendRequestLinkEvent because that one is the link slice's
// — the package's precedent (insertRequestEditEvent) is one append per concern,
// each owning the row it explains.
func insertRequestTransitionEvent(ctx context.Context, tx pgx.Tx, req *domain.TeamRequest,
	w RequestTransitionWrite, comment *domain.RequestComment, detached *int) error {
	ev := domain.RequestEvent{
		RequestID:   req.ID,
		OrgID:       req.OrgID,
		TeamID:      req.TeamID,
		Action:      domain.RequestActionTransition,
		FromStatus:  w.From,
		ToStatus:    w.To,
		Actor:       w.Actor,
		ActorUserID: w.ActorUserID,
	}
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("record transition event for request %d: %w", req.ID, err)
	}
	detail := transitionEventDetail{DetachedDuplicateOf: detached}
	if comment != nil {
		detail.CommentID = comment.ID
		detail.AnswerTo = w.Comment.AnswerTo
	}
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO request_events
		(request_id, org_id, team_id, action, from_state, to_state, actor, actor_user_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		ev.RequestID, ev.OrgID, ev.TeamID, string(ev.Action),
		string(ev.FromStatus), string(ev.ToStatus), ev.Actor, ev.ActorUserID,
		requestEventDetail(detail),
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("record transition event for request %d: %w", req.ID, err)
	}
	return nil
}
