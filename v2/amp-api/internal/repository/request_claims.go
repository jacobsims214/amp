package repository

// Team requests: the triage claim and the assignee (epic 202, story 535, AMP
// #3872; approved product contract artifact 36 v2 §1 and §2, defaults D4 and
// #3654 C1/C5; build slice C2 of
// discoveries/team-inbox-approved-build-slices.md).
//
// A request carries two ownership fields with different meanings, and this file
// is where each one changes:
//
//	claim_user_id / claimed_at  "I am looking at it right now": soft, one holder
//	                            at a time, released freely, never expired by a
//	                            clock (D4) and never taken from its holder.
//	owner_user_id               the assignee who owns the outcome: durable, moved
//	                            explicitly, and a transfer clears the claim.
//
// They are kept apart because conflating them is what made "mine" ambiguous in
// the human-work redesign: the shared queue needs to show who is triaging, and a
// person's own work needs to show what they own, and those are different
// answers. ClaimRequest changes the first, SetAssignee the second (clearing the
// first, atomically, in the same statement), ReleaseClaim ends the first.
//
// Three properties this layer is responsible for, and how each is kept:
//
//  1. Exactly one claim holder (I4). The decision "is it claimed?" and the write
//     that claims it happen under one row lock in one transaction: SELECT ... FOR
//     UPDATE through the shared scanner, then UPDATE ... RETURNING. Two triagers
//     claiming at once therefore serialise on the row, and the second one sees
//     the first one's claim and is refused (RequestClaimHeld names the holder)
//     instead of both reporting success. No read-then-write window exists to
//     lose.
//
//  2. No expiry, no steal. Nothing here compares claimed_at to a clock, so a
//     claim never lapses (D4: the UI shows claim age; a human claim is not a dead
//     worker). A claim held by someone else is never overwritten: taking work
//     from its holder is an explicit assignee transfer, which is SetAssignee's
//     job and which clears the claim, or a release by the holder.
//
//  3. A replay writes nothing. Each operation first asks whether the row already
//     says what the caller is asking for — the same claimant, no claim, the same
//     assignee with no claim — and answers "already so" with the row, without
//     bumping rev and without a second request_events row. That is what makes a
//     retry safe (a lost response, a double-click) and what keeps history one row
//     per act (I6). Only a call that changes the row bumps rev, and only that
//     call appends history.
//
// What this file deliberately does NOT do: decide who may claim, release or
// assign (the service's authorization gate, slice S4), move the request's state
// (the one transition function, slice S5), or write audit_log rows (the service,
// which owns the audit context). A claim is not a transition: it changes who is
// looking, not where the request is.
//
// Every read and write here goes through request_rows.go's contract —
// requestSelect, requestTenantWhere, requestCols and scanRequest — and does not
// edit it. The tenant is required on all three operations and is re-stated in
// the UPDATE, so a call with another team's ids writes nothing at all and is
// answered exactly like an id that does not exist (ErrNotFound, the request
// surface's one 404).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/simstech/amp-api/internal/domain"
)

// ErrRequestClaimant marks a claim write made without naming whose claim it is.
// Like ErrRequestTenantScope it reports a caller bug rather than a refusal worth
// showing anyone: a claim says who is taking the request and a release says whose
// claim is ending, so a write that named nobody could record (or end) one person's
// act under another's name. It is deliberately not ErrConflict and never ErrNotFound.
var ErrRequestClaimant = errors.New("a claim write names its claimant: pass the user the claim belongs to")

// RequestClaimHeld reports a claim or release refused because someone else holds
// the request's triage claim. It carries the holder so a caller can name them
// (the 409 the service answers names who won the race) without a second read, and
// matches ErrConflict, so a handler that only distinguishes 409 from success
// needs no special case.
//
// Nothing was written when this is returned: the holder's claim, the rev and the
// history are all untouched. Take-over is an assignee transfer, not a claim.
type RequestClaimHeld struct {
	RequestID int
	Holder    int
}

func (e *RequestClaimHeld) Error() string {
	return fmt.Sprintf("request %d is claimed by user %d: a claim is released by its holder or cleared by an assignee transfer, never taken", e.RequestID, e.Holder)
}

// Is makes errors.Is(err, ErrConflict) true for a lost claim.
func (e *RequestClaimHeld) Is(target error) bool { return target == ErrConflict }

// RequestRevConflict reports a claim write refused because the caller's expected
// rev is stale: the request changed since it was read. Current is the rev the row
// is actually at, so a caller can say so (and tell a client which If-Match to
// retry with) without re-reading, and the write did not happen.
//
// It matches ErrConflict for the same reason RequestClaimHeld does.
type RequestRevConflict struct {
	RequestID int
	Expected  int
	Current   int
}

func (e *RequestRevConflict) Error() string {
	return fmt.Sprintf("request %d is at rev %d, not %d: it changed since it was read, so reload it and reapply", e.RequestID, e.Current, e.Expected)
}

// Is makes errors.Is(err, ErrConflict) true for a stale rev.
func (e *RequestRevConflict) Is(target error) bool { return target == ErrConflict }

// RequestClaimWrite is the caller's part of a claim, release or assignee change:
// which request in which tenant, who is doing it, what they believe about the row,
// and whose claim or assignee the operation is about. The three operations share
// one shape because they share one transaction; each uses the fields it needs and
// ignores the rest (ClaimUserID for claim and release, Assignee for SetAssignee).
type RequestClaimWrite struct {
	// The request and its tenant. Both halves of the tenant are required: a
	// write scoped by only one of them would be a cross-team (or cross-org)
	// write primitive.
	RequestID int
	OrgID     int
	TeamID    int

	// ClaimUserID is the claimant: who is taking the request (ClaimRequest) or
	// whose claim is ending (ReleaseClaim). SetAssignee does not read it — a
	// transfer is not one person's claim.
	ClaimUserID int

	// Assignee is the owner to set (SetAssignee only). A nil pointer unassigns:
	// the request has no owner, which is a state a request can legitimately be
	// put back into.
	Assignee *int

	// ExpectRev, when greater than zero, requires the stored rev to equal it:
	// the caller's compare-and-set. Zero means "whatever the row is now", which
	// is what a replay-tolerant caller passes. The check applies only to a call
	// that would change something (see the replay rule above): a call that finds
	// the row already as asked writes nothing, so there is no lost update for a
	// stale rev to protect against, and a retry succeeds instead of conflicting.
	ExpectRev int

	// Actor is who the change is attributed to in the request's history, and
	// ActorUserID the AMP user behind that name when there is one. A blank
	// actor is recorded as "system" (history has no anonymous rows); the service
	// passes the principal, which may be a lead acting on someone else's claim.
	Actor       string
	ActorUserID *int
}

// ClaimRequest takes request w names for w.ClaimUserID.
//
// It succeeds only when the request is unclaimed, or when w.ClaimUserID already
// holds it (a retry, which changes nothing). A claim held by anyone else is
// RequestClaimHeld and writes nothing — there is no expiry and no take-over here
// (D4): releasing the claim or transferring the assignee is the way to take work
// over, and both are explicit acts by someone entitled to them.
//
// changed reports whether the row was written: true means the claim was taken,
// the rev bumped once and exactly one request_events row was appended; false means
// the request already looked like this and nothing at all was written.
func (r *Repo) ClaimRequest(ctx context.Context, w RequestClaimWrite) (req *domain.TeamRequest, changed bool, err error) {
	if err := checkRequestWriteScope(w.RequestID, w.OrgID, w.TeamID); err != nil {
		return nil, false, err
	}
	if w.ClaimUserID <= 0 {
		return nil, false, fmt.Errorf("claim request %d: %w", w.RequestID, ErrRequestClaimant)
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	req, err = lockRequest(ctx, tx, w)
	if err != nil {
		return nil, false, err
	}
	if req.ClaimUserID != nil {
		if *req.ClaimUserID == w.ClaimUserID {
			// Already claimed by this caller: a retry, or the same triager
			// clicking twice. Nothing to write, so nothing moves — no rev bump,
			// no second history row, and claimed_at keeps the age it had.
			return req, false, nil
		}
		return nil, false, &RequestClaimHeld{RequestID: req.ID, Holder: *req.ClaimUserID}
	}
	if err := checkRequestExpectRev(req, w.ExpectRev); err != nil {
		return nil, false, err
	}
	err = applyRequestClaimWrite(ctx, tx, req, w,
		`claim_user_id = $1, claimed_at = NOW()`, []any{w.ClaimUserID},
		domain.RequestActionClaim, claimEventDetail{ClaimUserID: w.ClaimUserID})
	if err != nil {
		return nil, false, requestUserRefError(err, w.ClaimUserID)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return req, true, nil
}

// ReleaseClaim ends the claim w.ClaimUserID holds on request w names.
//
// It is idempotent in the way a release should be: a request with no claim is
// already released, so the call succeeds with changed=false and writes nothing —
// no rev bump, no history row — which is what makes a retry after a lost response
// safe. Releasing a claim someone else holds is RequestClaimHeld and writes
// nothing; silently ending another person's claim would be the mirror of a claim
// steal.
func (r *Repo) ReleaseClaim(ctx context.Context, w RequestClaimWrite) (req *domain.TeamRequest, changed bool, err error) {
	if err := checkRequestWriteScope(w.RequestID, w.OrgID, w.TeamID); err != nil {
		return nil, false, err
	}
	if w.ClaimUserID <= 0 {
		return nil, false, fmt.Errorf("release claim on request %d: %w", w.RequestID, ErrRequestClaimant)
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	req, err = lockRequest(ctx, tx, w)
	if err != nil {
		return nil, false, err
	}
	if req.ClaimUserID == nil {
		return req, false, nil
	}
	if *req.ClaimUserID != w.ClaimUserID {
		return nil, false, &RequestClaimHeld{RequestID: req.ID, Holder: *req.ClaimUserID}
	}
	if err := checkRequestExpectRev(req, w.ExpectRev); err != nil {
		return nil, false, err
	}
	// claim_user_id and claimed_at move together: 053's CHECK refuses a claim
	// with no claim time, and one statement is what keeps that true.
	err = applyRequestClaimWrite(ctx, tx, req, w,
		`claim_user_id = NULL, claimed_at = NULL`, nil,
		domain.RequestActionRelease, releaseEventDetail{ReleasedClaimUserID: w.ClaimUserID})
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return req, true, nil
}

// SetAssignee makes w.Assignee the request's owner (nil unassigns it).
//
// The transfer and the claim it clears are one UPDATE, so there is no instant in
// which a request has a new owner and a stale claim (I4/#3654 C5), and the
// history row records both the previous owner and whose claim was cleared. A
// transfer is how work is taken from a busy claimant: it is an explicit act, and
// it leaves a trace, rather than a silent claim steal.
//
// changed=false means the row already is (w.Assignee, unclaimed) — the state the
// call asks for — so nothing was written: no rev bump, no second event, and an
// assignee transfer replayed does not read as a second transfer.
func (r *Repo) SetAssignee(ctx context.Context, w RequestClaimWrite) (req *domain.TeamRequest, changed bool, err error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	req, err = lockRequest(ctx, tx, w)
	if err != nil {
		return nil, false, err
	}
	if sameOptionalID(req.OwnerUserID, w.Assignee) && req.ClaimUserID == nil {
		// Owner already as asked and nobody triaging it: the requested state is
		// in force, so this is a replay and writes nothing.
		return req, false, nil
	}
	if err := checkRequestExpectRev(req, w.ExpectRev); err != nil {
		return nil, false, err
	}
	// Recorded before the update overwrites them: what the history row explains
	// is the change, so it names where the request came from.
	detail := assignEventDetail{PreviousOwnerUserID: req.OwnerUserID, ClearedClaimUserID: req.ClaimUserID}
	err = applyRequestClaimWrite(ctx, tx, req, w,
		`owner_user_id = $1, claim_user_id = NULL, claimed_at = NULL`, []any{w.Assignee},
		domain.RequestActionAssign, detail)
	if err != nil {
		if w.Assignee == nil {
			return nil, false, err
		}
		return nil, false, requestUserRefError(err, *w.Assignee)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return req, true, nil
}

// ---- The transaction body the three operations share ----

// checkRequestWriteScope refuses a write that cannot name the row it means, with
// the two errors the read path uses (request_rows.go's getRequest): a
// non-positive request id names no row and is an ordinary missing request, while a
// missing org or team is a caller bug that must not be answered as a 404. The
// tenant predicate itself is never written here — lockRequest and
// applyRequestClaimWrite both use requestTenantWhere's rule.
//
// Every operation runs it first, before it checks anything else: which row a write
// is about is a more basic question than whose claim it is, and a write with a
// broken scope must never reach the database (or, worse, be refused in a way that
// says something about a row it was never allowed to address).
func checkRequestWriteScope(requestID, orgID, teamID int) error {
	if requestID <= 0 {
		return fmt.Errorf("request %d: %w", requestID, ErrNotFound)
	}
	if orgID <= 0 || teamID <= 0 {
		return fmt.Errorf("request %d: %w", requestID, ErrRequestTenantScope)
	}
	return nil
}

// lockRequest loads the request w names and holds its row until the transaction
// ends, so the decision a claim makes about it is still true when the claim
// writes. It reads through the shared contract (requestSelect, requestTenantWhere,
// scanRequest) with FOR UPDATE appended: one column list, one tenant predicate,
// one scanner for reads, locks and writes alike.
//
// A request in another team or org, and one that does not exist, are both
// ErrNotFound — the request surface has one 404, and a wrong tenant must not be
// distinguishable from a missing id.
func lockRequest(ctx context.Context, tx pgx.Tx, w RequestClaimWrite) (*domain.TeamRequest, error) {
	if err := checkRequestWriteScope(w.RequestID, w.OrgID, w.TeamID); err != nil {
		return nil, err
	}
	req := &domain.TeamRequest{}
	err := scanRequest(tx.QueryRow(ctx, requestSelect+requestTenantWhere+` FOR UPDATE`,
		w.RequestID, w.OrgID, w.TeamID), req)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("request %d: %w", w.RequestID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("lock request %d: %w", w.RequestID, err)
	}
	return req, nil
}

// checkRequestExpectRev applies the caller's compare-and-set, when it asked for
// one. expect == 0 is "no precondition" and always passes; a rev the row has moved
// past is RequestRevConflict carrying the rev it is actually at.
func checkRequestExpectRev(req *domain.TeamRequest, expect int) error {
	if expect == 0 || expect == req.Rev {
		return nil
	}
	return &RequestRevConflict{RequestID: req.ID, Expected: expect, Current: req.Rev}
}

// applyRequestClaimWrite writes one claim/assignee change and appends its one
// history row, inside the caller's transaction: either both land or neither does.
//
// set is the change's SET clause, with placeholders $1..$len(values); the rev bump
// and updated_at are added here, once, so every claim write moves the concurrency
// token the same way and no caller can forget it. The tenant is re-stated in the
// UPDATE's predicate and the updated row is read back through the shared column
// list, so this can never write (or return) a row outside the request's org and
// team even though the row is already locked.
//
// Nothing is written before the UPDATE: a refused change (a lost claim, a stale
// rev) has already returned, so history never records an act that did not happen.
func applyRequestClaimWrite(ctx context.Context, tx pgx.Tx, req *domain.TeamRequest, w RequestClaimWrite,
	set string, values []any, action domain.RequestEventAction, detail any) error {
	n := len(values)
	args := make([]any, 0, n+3)
	args = append(append(args, values...), req.ID, req.OrgID, req.TeamID)
	sql := `UPDATE team_requests SET ` + set + `, rev = rev + 1, updated_at = NOW()` +
		fmt.Sprintf(` WHERE id = $%d AND org_id = $%d AND team_id = $%d`, n+1, n+2, n+3) +
		` RETURNING ` + requestCols
	if err := scanRequest(tx.QueryRow(ctx, sql, args...), req); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The row was locked a statement ago in this transaction, so a
			// vanished row means the tenant or id did not match after all:
			// answer it as the read path would, not as a database failure.
			return fmt.Errorf("request %d: %w", req.ID, ErrNotFound)
		}
		return fmt.Errorf("update request %d: %w", req.ID, err)
	}
	return insertRequestEvent(ctx, tx, req, w, action, detail)
}

// insertRequestEvent appends the one history row an act gets (I6), in the
// transaction that made the change. A claim, a release and a transfer are not
// transitions, so from_state/to_state stay empty (054's CHECK) and the detail
// explains the act instead.
func insertRequestEvent(ctx context.Context, tx pgx.Tx, req *domain.TeamRequest, w RequestClaimWrite,
	action domain.RequestEventAction, detail any) error {
	actor := strings.TrimSpace(w.Actor)
	if actor == "" {
		// History has no anonymous rows (054's actor CHECK). An act whose caller
		// passed no identity is recorded as the system's; the service passes the
		// principal behind it.
		actor = "system"
	}
	var id int64
	err := tx.QueryRow(ctx,
		`INSERT INTO request_events (request_id, org_id, team_id, action, actor, actor_user_id, detail)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		req.ID, req.OrgID, req.TeamID, string(action), actor, w.ActorUserID, requestEventDetail(detail),
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("record %s event for request %d: %w", action, req.ID, err)
	}
	return nil
}

// The history details. They carry ids only — never a name, an email or request
// content — because a request's history is requester-visible, and an id that the
// reader may not resolve is a smaller leak than a display name they may not see.
// A pointer field renders as null, which is the honest record that there was no
// previous owner / no claim to clear.
type claimEventDetail struct {
	ClaimUserID int `json:"claim_user_id"`
}

type releaseEventDetail struct {
	ReleasedClaimUserID int `json:"released_claim_user_id"`
}

type assignEventDetail struct {
	PreviousOwnerUserID *int `json:"previous_owner_user_id"`
	ClearedClaimUserID  *int `json:"cleared_claim_user_id"`
}

// requestEventDetail renders a history detail as JSON. The values it is given are
// ids and nothing else, so the encode cannot fail; an empty detail is a safe
// rendering of a bug rather than a reason to fail the write the detail explains.
func requestEventDetail(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// requestUserRefError turns a foreign-key refusal on a user reference into the
// ordinary not-found for that user. The only foreign keys a claim or assignee
// write touches are to users (053: claim_user_id, owner_user_id), so a 23503 here
// can mean nothing else — and naming the user is what lets a caller answer "no
// such user" instead of reporting a constraint violation as a server fault.
func requestUserRefError(err error, userID int) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		return err
	}
	return fmt.Errorf("user %d: %w", userID, ErrNotFound)
}

// sameOptionalID compares two nullable user references by value: nil equals nil,
// and two pointers to the same id are the same person.
func sameOptionalID(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
