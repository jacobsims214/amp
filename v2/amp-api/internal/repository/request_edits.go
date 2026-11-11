package repository

// Team requests: the versioned content edit (epic 202, story 535, AMP #3873;
// approved product contract artifact 36 v2 §1: "versioned edits (stale write →
// 409)"; build slice C3 of discoveries/team-inbox-approved-build-slices.md).
//
// UpdateRequest is the ONE write that changes what a request says. It is a
// compare-and-set on team_requests.rev: the caller says which revision it read,
// the row is locked, and the write either lands on that exact revision or is
// refused. A stale revision is never a lost update — it is a conflict that
// carries the revision the row is actually at, so the caller can re-read and
// decide, and a person who edited while an agent was still planning never has
// their words silently replaced.
//
// WHAT IS EDITABLE IS A TYPE, NOT A FILTER. RequestEdit has fields for exactly
// four things — subject, input_brief, outcome_brief and metadata labels — and
// the UPDATE below names exactly those columns. State, the triage claim, the
// assignee, duplicate_of, reject_reason, the lifecycle timestamps,
// reopened_count, public_ref, org/team, the requester snapshot and the source
// identity have no field to put a value in, so no payload reachable through this
// function can move a request's lifecycle or rewrite who asked. "Immutable
// identity and state cannot be overwritten" is a property of the shape of the
// write, not a check somebody has to remember to keep. Lifecycle changes belong
// to the one transition function (slice S5) and claims/assignment to their own
// CAS statements (slice C2); both are deliberately absent here.
//
// THE ROW CONTRACT IS REUSED, NOT COPIED. Reads go through requestSelect /
// requestTenantWhere / scanRequest / getRequest from request_rows.go, unchanged
// — that file's header says later slices use it and must not edit it, and this
// slice keeps that promise: one column list, one scanner, one tenant predicate,
// so an edit returns exactly the shape every other read returns.
//
// THE EDIT'S HISTORY IS ONE request_events ROW OF ACTION `edit`. The approved
// contract says an edit updates the current revision atomically WITH an edit
// event, and 054's split puts an act in the team's history rail, not in the
// requester's thread — so the row is appended to request_events in the same
// transaction as the UPDATE and the rev bump, and no request_comments row is
// written. Migration 062 widened 054's closed seven-verb CHECK with `edit` and
// domain.RequestEventActions mirrors it (#3754's rule: DB and domain may not
// disagree); AMP #3873 shipped a `system` comment only because 061 was the
// story's sole migration writer and no slot was free.
//
// The row carries the actor (display snapshot plus nullable AMP user), empty
// from_state/to_state (054's request_events_transition_states rule: only a
// `transition` names states, so an edit can never fake a lifecycle move), and a
// detail of the changed FIELD NAMES and the revision it moved between — never
// the brief text, because a request's history is requester-visible.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// RequestEdit is the payload of one versioned content edit. Its fields ARE the
// editable surface of a request; anything absent here cannot be changed by an
// edit (see the file header).
//
// Every string is optional-by-nil: nil leaves that field exactly as stored,
// while a non-nil pointer to "" clears it — which is how a team withdraws an
// outcome brief it no longer stands behind. Subject is the exception that
// validation refuses when blank (053 requires it), so `Subject: &""` is an
// error rather than a way to erase the one-line summary.
type RequestEdit struct {
	// Subject is the one-line summary (domain.MaxRequestSubjectLen).
	Subject *string
	// InputBrief is the requester's markdown (domain.MaxRequestBriefLen).
	InputBrief *string
	// OutcomeBrief is the team's delivery note (domain.MaxRequestBriefLen).
	OutcomeBrief *string
	// Metadata is a per-key label change: keys with a value are set, keys with
	// nil are removed, keys not named are kept (domain.MetadataPatch.Split).
	// Integrations label requests through here, so a patch from one connector
	// cannot drop another's keys by omission.
	Metadata domain.MetadataPatch
}

// Empty reports whether the edit names nothing to change. An empty edit is not
// an error — it is a no-op that returns the stored row unchanged, so a caller
// that diffed a form and found nothing to send does not churn the revision.
func (e RequestEdit) Empty() bool {
	return e.Subject == nil && e.InputBrief == nil && e.OutcomeBrief == nil && len(e.Metadata) == 0
}

// A stale revision is reported with the repository's shared RequestRevConflict
// (request_claims.go, slice C2): the same type answers a lost claim and a stale
// edit because both are the same refusal — "this row changed since you read it" —
// and one error shape means the service and REST layers map it once. Its Current
// is the revision the row is actually at, which is what this slice's contract
// promises and what a client needs to retry with the right If-Match.

// RequestEditActor is who made an edit, for the note the edit writes. Name is
// the display snapshot that keeps the row readable after the account is deleted
// (054's attribution rule); UserID links it to the account when there is one, and
// is nil for a connector or a service client with no AMP user.
type RequestEditActor struct {
	Name   string
	UserID *int
}

// UpdateRequest applies edit to the request requestID in orgID's teamID,
// compare-and-set against expectedRev, and returns the stored row after it was
// updated. It is one transaction: the row is locked, the revision is checked, the
// content is written with the revision bumped exactly once, the history note is
// appended, and the whole thing commits — or nothing does.
//
// The outcomes, and keeping them apart is the point of the signature:
//
//	| Situation                                              | Returns |
//	|--------------------------------------------------------|---------|
//	| The row exists in this tenant at expectedRev            | the updated row, changed=true |
//	| ... and the edit changes nothing                        | the stored row, changed=false, no bump, no note |
//	| The row exists in this tenant at another rev            | nil, false, *RequestRevConflict (matches ErrConflict) |
//	| Another team's row, another org's row, an unknown id    | nil, false, ErrNotFound |
//	| Missing tenant (0) or an expected rev below 1           | nil, false, ErrRequestTenantScope |
//	| A real load/write failure (ctx cancelled, connection)   | wrapped as itself |
//
// A conflict and a missing row are deliberately distinguishable: the request
// surface answers one 404 for every invisible row (slice B4) and a 409 for a
// conflict the caller can actually resolve, so a wrong tenant stays
// indistinguishable from a missing request while a stale revision is told the
// truth about a row the caller is allowed to see.
//
// The lock is SELECT … FOR UPDATE through getRequest rather than an optimistic
// retry loop: two concurrent editors serialise on the row, so the second reads
// the revision the first committed and gets an honest conflict instead of both
// computing a bump from the same stale read.
func (r *Repo) UpdateRequest(ctx context.Context, requestID, orgID, teamID, expectedRev int, edit RequestEdit, by RequestEditActor) (*domain.TeamRequest, bool, error) {
	if orgID <= 0 || teamID <= 0 {
		// Same caller bug getRequest names: an edit with no tenant is a mistake,
		// not a missing row, and answering 404 would hide it behind a legitimate
		// refusal. Checked here too because this path writes, not just reads.
		return nil, false, fmt.Errorf("update request %d: %w", requestID, ErrRequestTenantScope)
	}
	if expectedRev < 1 {
		// rev starts at 1 (053's CHECK), so a lower expected revision cannot name
		// any stored row. That is a caller bug, not a conflict: "the row is at N"
		// would suggest a retry that can never win.
		return nil, false, fmt.Errorf("update request %d: expected rev must be at least 1, got %d: %w",
			requestID, expectedRev, ErrRequestTenantScope)
	}
	if by.Name == "" {
		by.Name = "system"
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("update request %d: %w", requestID, err)
	}
	defer tx.Rollback(ctx)

	// The row this transaction is about to change, LOCKED, tenant-scoped and read
	// through the one scanner. The lock is what makes the compare-and-set honest:
	// without FOR UPDATE two editors both read rev 1, both pass the check below,
	// and the loser's UPDATE matches zero rows — which this code would have to
	// report as "not found" when the row plainly exists. With the lock, the second
	// editor blocks until the first commits, then reads rev 2 and gets a real
	// conflict it can resolve. requestTenantWhere and scanRequest are reused
	// unchanged; only the trailing lock is added, exactly as request_rows.go's
	// header prescribes for the write slices.
	current := &domain.TeamRequest{}
	err = scanRequest(tx.QueryRow(ctx, requestSelect+requestTenantWhere+` FOR UPDATE`,
		requestID, orgID, teamID), current)
	if errors.Is(err, pgx.ErrNoRows) {
		// The row is not in this tenant (or does not exist). One 404 for both, the
		// same answer getRequest gives, so an id walker learns nothing.
		return nil, false, fmt.Errorf("request %d: %w", requestID, ErrNotFound)
	}
	if err != nil {
		return nil, false, fmt.Errorf("update request %d: lock request: %w", requestID, err)
	}
	if current.Rev != expectedRev {
		return nil, false, &RequestRevConflict{RequestID: requestID, Expected: expectedRev, Current: current.Rev}
	}

	// Validate and normalise before writing: a rejected field never bumps the
	// revision, and an edit cannot store a subject, brief or label the create path
	// would have refused.
	next, changedFields, err := applyRequestEdit(current, edit)
	if err != nil {
		return nil, false, fmt.Errorf("update request %d: %w", requestID, err)
	}
	if len(changedFields) == 0 {
		// Nothing to write. Returning the stored row without bumping rev keeps a
		// repeated save from making every other holder of the revision stale.
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("update request %d: %w", requestID, err)
		}
		return current, false, nil
	}

	// One statement writes the content and the new revision together. The WHERE
	// clause repeats the tenant AND the expected revision even though the row is
	// locked above: the lock proves the read was consistent, and the predicate
	// proves the write is the one that was decided on — a future reader cannot
	// drop the lock and silently turn this into a last-write-wins update.
	// RETURNING goes through the shared scanner, so the caller gets the stored
	// row rather than a locally patched guess.
	updated := &domain.TeamRequest{}
	err = scanRequest(tx.QueryRow(ctx, `
		UPDATE team_requests SET subject = $1, input_brief = $2, outcome_brief = $3,
		       metadata = $4, rev = $5, updated_at = NOW()
		WHERE id = $6 AND org_id = $7 AND team_id = $8 AND rev = $9
		RETURNING `+requestCols,
		next.Subject, next.InputBrief, next.OutcomeBrief, next.Metadata, next.Rev,
		requestID, orgID, teamID, expectedRev), updated)
	if errors.Is(err, pgx.ErrNoRows) {
		// The row cannot have vanished under a held lock, so zero rows here means the
		// revision moved between the lock and the write in a way this transaction
		// cannot see (a deferred/autonomous writer). Report it as the conflict it is:
		// a 404 would tell the caller their request does not exist when it does.
		return nil, false, &RequestRevConflict{RequestID: requestID, Expected: expectedRev, Current: current.Rev}
	}
	if err != nil {
		return nil, false, fmt.Errorf("update request %d: %w", requestID, err)
	}

	// The edit event is appended in the same transaction as the write, so the
	// history can never show an edit whose content did not land, or miss one that
	// did — and a failed insert rolls the content and the revision back with it.
	// It names the fields and the revision transition, never the text.
	if err := insertRequestEditEvent(ctx, tx, updated, by, changedFields, expectedRev, next.Rev); err != nil {
		return nil, false, fmt.Errorf("update request %d: %w", requestID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("update request %d: %w", requestID, err)
	}
	return updated, true, nil
}

// applyRequestEdit returns the row as it would be stored after edit, the fields
// that actually changed (in a stable order) and any validation error. It is a
// pure function of the stored row and the payload: it never touches the database,
// which is what makes the no-op and the refusal cases cheap and testable.
//
// It validates the RESULT through domain.TeamRequest.Validate, not just the
// fields named, because an edit can break a whole-row invariant no single field
// check can see — clearing outcome_brief on a delivered request would violate
// 053's "delivered is evidenced" CHECK, and a pg constraint message is a worse
// answer than the domain's own sentence.
func applyRequestEdit(current *domain.TeamRequest, edit RequestEdit) (*domain.TeamRequest, []string, error) {
	next := *current // value copy; the maps and pointers are replaced below, never mutated
	next.Metadata = cloneRequestMetadata(current.Metadata)
	var changed []string

	if edit.Subject != nil {
		subject, err := domain.NormalizeRequestSubject(*edit.Subject)
		if err != nil {
			return nil, nil, err
		}
		if subject != current.Subject {
			next.Subject = subject
			changed = append(changed, "subject")
		}
	}
	if edit.InputBrief != nil {
		brief, err := domain.NormalizeRequestBrief("input_brief", *edit.InputBrief)
		if err != nil {
			return nil, nil, err
		}
		if brief != current.InputBrief {
			next.InputBrief = brief
			changed = append(changed, "input_brief")
		}
	}
	if edit.OutcomeBrief != nil {
		brief, err := domain.NormalizeRequestBrief("outcome_brief", *edit.OutcomeBrief)
		if err != nil {
			return nil, nil, err
		}
		if brief != current.OutcomeBrief {
			next.OutcomeBrief = brief
			changed = append(changed, "outcome_brief")
		}
	}
	if len(edit.Metadata) > 0 {
		// Split validates key shapes and value types and separates the removals,
		// which is exactly the patch semantics the type documents.
		set, remove, err := edit.Metadata.Split()
		if err != nil {
			return nil, nil, err
		}
		for _, key := range remove {
			if _, had := next.Metadata[key]; had {
				delete(next.Metadata, key)
				changed = append(changed, "metadata."+key)
			}
		}
		keys := make([]string, 0, len(set))
		for key := range set {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			before, had := next.Metadata[key]
			if had && metadataValueEqual(before, set[key]) {
				continue
			}
			next.Metadata[key] = set[key]
			changed = append(changed, "metadata."+key)
		}
		// Metadata's documented limit is per change; a merged row past the whole
		// object's bound would be a new way to exceed it, so refuse it here rather
		// than growing a JSONB value without limit.
		if len(next.Metadata) > domain.MaxMetadataKeys {
			return nil, nil, fmt.Errorf("metadata: at most %d keys on a request", domain.MaxMetadataKeys)
		}
	}
	if len(changed) == 0 {
		return current, nil, nil
	}

	next.Rev = current.Rev + 1
	if err := next.Validate(); err != nil {
		return nil, nil, err
	}
	return &next, changed, nil
}

// cloneRequestMetadata copies a request's labels so applying a patch cannot
// mutate the row the caller passed in (the nil map becomes empty, which is what
// the column's object CHECK needs).
func cloneRequestMetadata(m domain.Metadata) domain.Metadata {
	out := make(domain.Metadata, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// metadataValueEqual compares one label value. JSONB numbers come back as
// float64, but a patch may carry an int (a caller building a value in Go), so the
// numeric kinds compare by value rather than by Go type; strings and booleans
// compare directly. Anything else is not a value Metadata.Split would have
// accepted, so it is honestly "not equal" rather than a silent no-op.
func metadataValueEqual(a, b any) bool {
	if af, ok := asFloat(a); ok {
		bf, bOK := asFloat(b)
		return bOK && af == bf
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}
	return false
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	}
	return 0, false
}

// editEventDetail is what one edit history row explains: which fields moved (the
// editable surface's names, or "metadata.<key>") and the revision transition.
// Field names and numbers only — never a subject or a brief, because a request's
// history is requester-visible and copying content into it would need its own
// redaction rule.
type editEventDetail struct {
	Fields  []string `json:"fields"`
	FromRev int      `json:"from_rev"`
	ToRev   int      `json:"to_rev"`
}

// insertRequestEditEvent appends the one history row an edit gets (I6) — action
// `edit`, no states (054's CHECK allows states only on a transition) — in the
// transaction that made the change. It is its own function rather than a call
// into request_claims.go's insertRequestEvent because that helper's input is a
// claim write; this path owns its single event row, the same division
// request_create.go's submit row follows.
func insertRequestEditEvent(ctx context.Context, tx pgx.Tx, req *domain.TeamRequest,
	by RequestEditActor, fields []string, fromRev, toRev int) error {
	ev := domain.RequestEvent{
		RequestID:   req.ID,
		OrgID:       req.OrgID,
		TeamID:      req.TeamID,
		Action:      domain.RequestActionEdit,
		Actor:       by.Name,
		ActorUserID: by.UserID,
	}
	// Validate through the domain so this row cannot be the one place the domain
	// verb list and 054's CHECK disagree: it refuses an unknown action and any
	// state on a non-transition before the database would.
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("record edit event: %w", err)
	}
	if ev.Actor == "" {
		// History has no anonymous rows (054's actor CHECK); UpdateRequest already
		// defaults a nameless caller to "system", and this keeps the helper safe.
		ev.Actor = "system"
	}
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO request_events
		(request_id, org_id, team_id, action, from_state, to_state, actor, actor_user_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		ev.RequestID, ev.OrgID, ev.TeamID, string(ev.Action), string(ev.FromStatus), string(ev.ToStatus),
		ev.Actor, ev.ActorUserID,
		requestEventDetail(editEventDetail{Fields: fields, FromRev: fromRev, ToRev: toRev}),
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("record edit event for request %d: %w", req.ID, err)
	}
	return nil
}
