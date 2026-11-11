package repository

// Team requests: the durable request-to-work lineage and the cycle-safe duplicate
// record (epic 202, story 535, AMP #3875; approved product contract artifact 36 v2
// §3, defaults D7 and D9; build slice C4 of
// discoveries/team-inbox-approved-build-slices.md).
//
// Migration 055 (request_links) is the table; this file is the only write path to
// it. It implements the four rules the schema deliberately left to Go — see 055's
// header, "WHAT IS NOT ENFORCED HERE, BY DESIGN":
//
//  1. A link's target is in the request's tenant. The target carries no foreign key
//     (a link outlives the ticket it names, so the row must survive as a tombstone),
//     so tenancy cannot be a constraint. Here the caller supplies TargetOrgID — the
//     org it resolved from its own record when it looked the target up (the
//     project, epic, story, task, artifact, receipt or request row it already had
//     to read to authorize the act) — and this layer verifies it twice: the supplied
//     org must match the org the target's table actually says, and the target's org
//     must be the request's. Metadata is never consulted. A JSONB key is inherited,
//     a metadata patch can rewrite it, and a value nobody verifies is not a tenant
//     check (domain/metadata.go's merge_for rule; 055's "tenant safety moves to the
//     link service"). A target in another org is answered exactly like a target that
//     does not exist (ErrNotFound), because the request surface has one 404 and a
//     wrong tenant must not be distinguishable from a missing id.
//
//  2. Attaching the same relation twice is one row, and a replay writes nothing
//     (055's unique key, the task_dependencies pattern). The insert rides
//     `ON CONFLICT DO NOTHING`, and created_by/created_at on the first row are kept
//     (first-write-wins, the merged_at rule). The part a bare ON CONFLICT gets
//     wrong: a replay appends no second history row and does not re-stamp
//     planned_at, so "did this link exist?" and "who first attached it?" answer the
//     same on the tenth attempt as on the first.
//
//  3. A duplicate fold is one act and never leaves a cycle (D7, #3654 C2). Two
//     triagers marking A a duplicate of B and B a duplicate of A concurrently both
//     commit if the service only checks first, so MarkDuplicate locks every request
//     row the act touches in ascending id order (the kb.go FOR UPDATE pattern,
//     ordered so two writers cannot deadlock), re-reads them under the lock,
//     flattens the named canonical to the root of its chain (a canonical is never a
//     duplicate), and refuses self, cycle, an already-folded request and a request
//     that has duplicates of its own. The typed column, the link row, the state
//     change and exactly one history row are one transaction, so no reader ever sees
//     a request whose state says duplicate and whose pointer is empty (053's CHECK
//     would refuse that) or a link row claiming a fold the record does not have.
//
//  4. A duplicate detach/reopen is atomic. DetachDuplicate clears duplicate_of,
//     leaves the duplicate state in the SAME update (the D7 rule, 053's CHECK),
//     records the reopen (reopened_at, reopened_count) and appends one transition
//     event — the one helper a reopen needs, not a lifecycle service.
//
// Routing is lineage, not state. A routes_to link is never replaced or deleted by a
// re-route: LinkRequest appends the new row and the previous one stays readable, and
// ActiveRequestRoute is the single answer to "which project is this request routed
// to now?" — newest row wins. D9's one-project rule is enforced at the write
// (a routes_to to a different project while one is active is RequestRouteConflict
// unless the caller explicitly supersedes it), which is a different question from
// "where has this request been routed?", answered by RequestLinks.
//
// What this file deliberately does NOT do: decide who may link, fold or detach (the
// service's authorization gate), run the lifecycle state machine (slice S5 — the
// only transitions here are the two a fold and its detach are), write audit_log rows
// (the service owns the audit context), or read the target's content. It adds no
// migration, index or column: 053's nine-index and 055's five-index proofs stay true.
//
// Every request read and write goes through request_rows.go's contract
// (requestSelect, requestTenantWhere, requestCols, scanRequest) and every single-row
// lock through request_claims.go's lockRequest, so the tenant predicate is written
// once in the package and no caller can forget half of it.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// ErrRequestLinkTargetIdentity marks a link write whose caller supplied no verified
// tenant for the target, or supplied one the target's own table disagrees with. Like
// ErrRequestTenantScope it reports a caller bug, not a refusal worth showing anyone:
// the org of the record being linked is what the tenant check compares against, so a
// link written with an org read out of metadata (or not resolved at all) is exactly
// the cross-tenant write the check exists to stop. It is deliberately not
// ErrConflict and never ErrNotFound.
var ErrRequestLinkTargetIdentity = errors.New("a link names the target's verified org: resolve the target from its own record and pass that org, never one read from metadata")

// ErrRequestLinkDuplicate marks a duplicate_of relation offered to the general link
// paths. A fold is not a link insert: it moves the request's duplicate_of column and
// its state in the same statement (053's CHECK keeps the pair from ever
// disagreeing), flattens the canonical and locks every row it touches. Writing the
// link alone would produce a row claiming a fold the record does not have, so
// LinkRequest and UnlinkRequest refuse this kind and MarkDuplicate/DetachDuplicate
// own the relation.
var ErrRequestLinkDuplicate = errors.New("a duplicate_of relation is written by MarkDuplicate and removed by DetachDuplicate: it moves the request's duplicate_of and state in the same statement")

// RequestRouteConflict reports a routes_to attach refused because the request is
// already routed to a different project (D9: one project per request). CurrentID is
// the project it is routed to now, so a caller can name it (the 409) without a
// second read, and nothing was written.
//
// A deliberate re-route is allowed and appends a row: the previous routing stays as
// history. That is what SupersedeRoute in RequestLinkWrite is for — the rule is
// enforced, not replaced, so no lineage is deleted.
type RequestRouteConflict struct {
	RequestID int
	CurrentID int
	WantedID  int
}

func (e *RequestRouteConflict) Error() string {
	return fmt.Sprintf("request %d is routed to project %d, not %d: a request has one project (D9), and a re-route supersedes the previous routing without deleting it", e.RequestID, e.CurrentID, e.WantedID)
}

// Is makes errors.Is(err, ErrConflict) true for a refused route.
func (e *RequestRouteConflict) Is(target error) bool { return target == ErrConflict }

// RequestDuplicateRefused reports a fold or detach refused because the requests
// cannot be related the way the caller asked. Reason is one of the
// duplicateRefused* values below and says which rule refused it. The type matches
// ErrConflict, so a handler that already answers 409 for a lost race needs no
// special case, and nothing was written: no duplicate_of, state, rev, link or
// history row moved.
type RequestDuplicateRefused struct {
	RequestID int    // the request the caller asked about
	TargetID  int    // the canonical the caller named (0 for a detach)
	Reason    string // which rule refused it (a duplicateRefused* constant)
}

// Is makes errors.Is(err, ErrConflict) true for a refused fold.
func (e *RequestDuplicateRefused) Is(target error) bool { return target == ErrConflict }

func (e *RequestDuplicateRefused) Error() string {
	switch e.Reason {
	case duplicateRefusedSelf:
		return fmt.Sprintf("request %d cannot be a duplicate of itself", e.RequestID)
	case duplicateRefusedCycle:
		return fmt.Sprintf("folding request %d into request %d would make the duplicate chain a cycle: the canonical flattens to this request", e.RequestID, e.TargetID)
	case duplicateRefusedFolded:
		return fmt.Sprintf("request %d is already a duplicate of another request: detach it before folding it into request %d", e.RequestID, e.TargetID)
	case duplicateRefusedHasDuplicates:
		return fmt.Sprintf("request %d has duplicates folded into it, so it cannot itself become a duplicate: fold or detach those first, so every duplicate points straight at a canonical", e.RequestID)
	case duplicateRefusedChainMoved:
		return fmt.Sprintf("request %d's duplicate chain kept changing while request %d was folded into it: reload and retry against the canonical it points at now", e.TargetID, e.RequestID)
	case duplicateRefusedChildElsewhere:
		return fmt.Sprintf("request %d has a duplicate in another team of its org, so it cannot be folded into request %d here: that duplicate has to be detached by its own team first", e.RequestID, e.TargetID)
	default:
		return fmt.Sprintf("request %d cannot be folded into request %d", e.RequestID, e.TargetID)
	}
}

// The reasons a fold is refused, named so an error, a test and a caller's 409 body
// all use the same words.
const (
	duplicateRefusedSelf           = "self"
	duplicateRefusedCycle          = "cycle"
	duplicateRefusedFolded         = "already_folded"
	duplicateRefusedHasDuplicates  = "has_duplicates"
	duplicateRefusedChainMoved     = "canonical_moved"
	duplicateRefusedChildElsewhere = "child_in_another_team"
)

// duplicateFoldAttempts bounds MarkDuplicate's retry when the canonical's chain
// moves between resolving it and taking the locks. The retry exists because the
// rows a fold locks are chosen from an unlocked read: two writers racing can each
// name a canonical the other has just made a duplicate. A retry re-resolves the
// chain and locks the new pair; a chain that keeps moving is a conflict, never an
// endless loop.
const duplicateFoldAttempts = 3

// maxDuplicateChain bounds how far a fold follows duplicate_of before declaring the
// chain broken. Every write in this file flattens, so a stored chain is one hop; the
// bound exists so a hand-edited row cannot make the resolver loop.
const maxDuplicateChain = 16

// errDuplicateRetry is internal to MarkDuplicate: the row set moved under the locks,
// so the attempt rolls back and resolves again. It never escapes this file.
var errDuplicateRetry = errors.New("duplicate chain moved under the lock")

// requestLinkCols is the column list of domain.RequestLink, in migration 055's
// order. One list and one scanner, for the reason requestCols exists: a link read
// that forgets a column reads as a zero value rather than as an error.
const requestLinkCols = `id, request_id, link_kind, target_kind, target_id, created_by, created_at`

// scanRequestLink scans one row of requestLinkCols.
func scanRequestLink(row rowScanner, l *domain.RequestLink) error {
	return row.Scan(&l.ID, &l.RequestID, &l.LinkKind, &l.TargetKind, &l.TargetID, &l.CreatedBy, &l.CreatedAt)
}

// requestLinkTargetOrgQueries resolves the org each link target actually belongs
// to, from the target's own table — never from metadata, and never from the
// request. `projects.org_id` is nullable (a pre-004 project), so its scan target is
// a pointer: a project with no org has no tenant to verify and is handled as a
// missing target.
var requestLinkTargetOrgQueries = map[domain.RequestTargetKind]string{
	domain.RequestTargetProject:  `SELECT org_id FROM projects WHERE id = $1`,
	domain.RequestTargetEpic:     `SELECT p.org_id FROM epics e JOIN projects p ON p.id = e.project_id WHERE e.id = $1`,
	domain.RequestTargetStory:    `SELECT p.org_id FROM stories s JOIN projects p ON p.id = s.project_id WHERE s.id = $1`,
	domain.RequestTargetTask:     `SELECT p.org_id FROM tasks t JOIN projects p ON p.id = t.project_id WHERE t.id = $1`,
	domain.RequestTargetArtifact: `SELECT p.org_id FROM artifacts a JOIN projects p ON p.id = a.project_id WHERE a.id = $1`,
	domain.RequestTargetReceipt:  `SELECT p.org_id FROM task_receipts r JOIN projects p ON p.id = r.project_id WHERE r.id = $1`,
	domain.RequestTargetRequest:  `SELECT org_id FROM team_requests WHERE id = $1`,
}

// RequestLinkWrite is the caller's part of an attach or a detach: which request in
// which tenant, the relation and its target, the target's verified tenant, and who
// did it.
type RequestLinkWrite struct {
	// The request and its whole tenant. Both halves are required: a write scoped by
	// only one of them would be a cross-team (or cross-org) write primitive.
	RequestID int
	OrgID     int
	TeamID    int

	LinkKind   domain.RequestLinkKind
	TargetKind domain.RequestTargetKind
	TargetID   int

	// TargetOrgID is the org the caller resolved for the target from the target's
	// OWN record — the row it read to authorize the act. It is required for an
	// attach and verified against the target's table before anything is written. A
	// detach ignores it: the row being removed is already inside the request's
	// tenant, and a tombstone's target may no longer exist to resolve.
	TargetOrgID int

	// SupersedeRoute allows a routes_to attach to a different project than the
	// request's active route (D9). Without it such an attach is
	// RequestRouteConflict. Superseding appends a row and deletes nothing: the
	// previous routing stays in the request's lineage.
	SupersedeRoute bool

	// CreatedBy is the AMP user who attached the link, when there is one. A
	// connector or MCP service client attaches without one, and deleting the user
	// later nulls it without removing the relation (055's ON DELETE SET NULL).
	CreatedBy *int

	// ExpectRev, when greater than zero, is a compare-and-set on the request's rev.
	// UnlinkRequest checks it; LinkRequest does not, on purpose: a link is lineage,
	// not a versioned edit of the request's content, so a caller that read rev 3
	// before attaching must not be told its brief is stale.
	ExpectRev int

	// Actor is who the act is attributed to in the request's history, and
	// ActorUserID the AMP user behind that name. A blank actor is recorded as
	// "system" (history has no anonymous rows, 054's CHECK).
	Actor       string
	ActorUserID *int
}

// RequestDuplicateWrite is the caller's part of a fold or a detach.
type RequestDuplicateWrite struct {
	// The request being folded (or detached) and its whole tenant.
	RequestID int
	OrgID     int
	TeamID    int

	// CanonicalID is the request the caller believes is the canonical. A fold
	// flattens it: if it is itself a duplicate, the fold lands on what it points at
	// (D7). A detach ignores it.
	CanonicalID int

	// ReopenTo is the state a detach returns the request to (D6: triaging or
	// needs_information; empty means triaging). It is read by DetachDuplicate only.
	ReopenTo domain.RequestStatus

	// CreatedBy is the AMP user who recorded the fold, when there is one.
	CreatedBy *int

	// ExpectRev, when greater than zero, is the caller's compare-and-set on the
	// folded request's rev.
	ExpectRev int

	Actor       string
	ActorUserID *int
}

// LinkRequest attaches one relation between a request and the work that serves it,
// and is idempotent.
//
// It returns the stored link and whether this call created it. changed=false means
// the relation was already there, so nothing at all was written — no second link
// row, no second history row, no planned_at stamp — and the returned link is the
// original one with its first attribution (created_by, created_at) intact. A replay
// after the target was deleted is still a replay: the tombstone answers, so
// re-attaching a relation whose target is gone does not manufacture a second row and
// does not fail.
//
// The target's tenancy is verified, never assumed. TargetOrgID must be the org the
// caller resolved from the target's own record, and it must match both what the
// target's table says and the request's org. A target that does not exist, or one in
// another org, is ErrNotFound (the request surface's one 404); a caller that
// supplied no verified org, or one the target disagrees with, is
// ErrRequestLinkTargetIdentity (a caller bug, not a refusal).
//
// A `planned_as` attach stamps the request's planned_at on a first attach only
// (first-write-wins; 053: "first planned_as link"). It does not bump rev: rev is the
// token for the request's versioned content, and a lineage stamp must not invalidate
// a client's copy of the brief. A `routes_to` attach obeys D9: a different project
// while one is active is RequestRouteConflict unless SupersedeRoute is set. A
// `duplicate_of` attach is ErrRequestLinkDuplicate — MarkDuplicate owns that
// relation because it moves the record with it.
func (r *Repo) LinkRequest(ctx context.Context, w RequestLinkWrite) (*domain.RequestLink, bool, error) {
	link, err := validatedRequestLink(w)
	if err != nil {
		return nil, false, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Lock the request: the attach decides whether the relation already exists, and
	// that decision has to still be true when the insert runs. Every link write for
	// one request therefore serialises here, which is also what makes the D9 route
	// check safe against a concurrent re-route.
	req, err := lockRequest(ctx, tx, RequestClaimWrite{RequestID: w.RequestID, OrgID: w.OrgID, TeamID: w.TeamID})
	if err != nil {
		return nil, false, err
	}

	// The replay check comes first, and before target resolution on purpose: a
	// relation already stored is already stored, even if the thing it names has
	// since been deleted (that is what a tombstone is). Re-resolving a gone target
	// would turn a harmless retry into a 404.
	existing, err := findRequestLink(ctx, tx, req.ID, w.OrgID, link.LinkKind, link.TargetKind, link.TargetID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}

	// A brand-new relation: the target has to exist and be in this tenant before the
	// row may claim it. This is the check the schema cannot make (no FK on
	// target_id) and the one metadata must never supply.
	if err := verifyRequestLinkTarget(ctx, tx, req, w, link); err != nil {
		return nil, false, err
	}

	var superseded *int
	if link.LinkKind == domain.RequestLinkRoutesTo {
		active, err := activeRequestRoute(ctx, tx, req.ID, req.OrgID)
		if err != nil {
			return nil, false, err
		}
		// Same project and no row found above cannot happen for a row this layer
		// wrote (the replay check would have answered), so it is left to the insert:
		// the unique key decides, and a phantom conflict is never reported.
		if active != nil && active.TargetID != link.TargetID {
			if !w.SupersedeRoute {
				return nil, false, &RequestRouteConflict{RequestID: req.ID, CurrentID: active.TargetID, WantedID: link.TargetID}
			}
			previous := active.TargetID
			superseded = &previous
		}
	}

	inserted, err := insertRequestLink(ctx, tx, req, link)
	if err != nil {
		return nil, false, requestLinkUserRefError(err, w.CreatedBy)
	}
	if !inserted {
		// Lost a race with a writer that does not take the request lock. The stored
		// row is the answer, first attribution and all, and nothing else is written.
		stored, err := findRequestLink(ctx, tx, req.ID, w.OrgID, link.LinkKind, link.TargetKind, link.TargetID)
		if err != nil {
			return nil, false, err
		}
		if stored == nil {
			return nil, false, fmt.Errorf("link %s %d to request %d vanished after a conflicting insert", link.TargetKind, link.TargetID, req.ID)
		}
		return stored, false, nil
	}

	if link.LinkKind == domain.RequestLinkPlannedAs {
		if _, err := tx.Exec(ctx, `UPDATE team_requests SET planned_at = COALESCE(planned_at, NOW())
			WHERE id = $1 AND org_id = $2 AND team_id = $3`, req.ID, req.OrgID, req.TeamID); err != nil {
			return nil, false, fmt.Errorf("stamp planned_at on request %d: %w", req.ID, err)
		}
	}
	if err := appendRequestLinkEvent(ctx, tx, req, domain.RequestActionLink, w.Actor, w.ActorUserID,
		"", "", linkEventDetail{
			LinkID: link.ID, LinkKind: string(link.LinkKind),
			TargetKind: string(link.TargetKind), TargetID: link.TargetID,
			SupersededProjectID: superseded,
		}); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return link, true, nil
}

// UnlinkRequest detaches one relation from a request without deleting either the
// request or the target, and is idempotent: a relation that is already gone is
// already detached, so the call answers changed=false and writes nothing (no rev
// bump, no history row), which makes a retry after a lost response safe.
//
// The row is deleted rather than marked, because 055 makes the link the live
// relation for every kind except duplication, and one `unlink` event keeps the act
// in history (I6). A duplicate_of relation is refused: DetachDuplicate owns it,
// because clearing the link there must clear the request's duplicate_of and leave
// its state in the same statement (053's CHECK).
//
// Detaching a routes_to row is how a team abandons a routing. It is deliberately
// ordinary here: D9's rule is about the ACTIVE route (see ActiveRequestRoute), and
// the usual way to change projects is a superseding attach, which keeps the old row.
func (r *Repo) UnlinkRequest(ctx context.Context, w RequestLinkWrite) (changed bool, err error) {
	link, err := validatedRequestLink(w)
	if err != nil {
		return false, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	req, err := lockRequest(ctx, tx, RequestClaimWrite{RequestID: w.RequestID, OrgID: w.OrgID, TeamID: w.TeamID})
	if err != nil {
		return false, err
	}
	if err := checkRequestExpectRev(req, w.ExpectRev); err != nil {
		return false, err
	}
	var id int64
	err = tx.QueryRow(ctx, `DELETE FROM request_links
		WHERE request_id = $1 AND link_kind = $2 AND target_kind = $3 AND target_id = $4 RETURNING id`,
		req.ID, string(link.LinkKind), string(link.TargetKind), link.TargetID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("detach %s %d from request %d: %w", link.TargetKind, link.TargetID, req.ID, err)
	}
	if err := appendRequestLinkEvent(ctx, tx, req, domain.RequestActionUnlink, w.Actor, w.ActorUserID,
		"", "", linkEventDetail{
			LinkID: id, LinkKind: string(link.LinkKind),
			TargetKind: string(link.TargetKind), TargetID: link.TargetID,
		}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// RequestLinks reads a request's whole lineage in attach order (`id`, not
// created_at, so rows written in one transaction keep a stable order — 055's plan
// rail). An optional kinds filter narrows the read (the delivery check wants
// delivered_by alone); no filter reads everything, tombstones included: a link whose
// target was deleted is still returned, because the relation is the record.
//
// The tenant is required and checked by loading the request itself, so a request in
// another team is ErrNotFound exactly as it is everywhere else in the package.
func (r *Repo) RequestLinks(ctx context.Context, requestID, orgID, teamID int, kinds ...domain.RequestLinkKind) ([]domain.RequestLink, error) {
	if len(kinds) > 0 {
		for _, k := range kinds {
			if !slices.Contains(domain.RequestLinkKinds, k) {
				return nil, fmt.Errorf("link kind %q is not one of %v", k, domain.RequestLinkKinds)
			}
		}
	}
	if _, err := getRequest(ctx, r.db, requestID, orgID, teamID); err != nil {
		return nil, err
	}
	// The request is already proved to be in this tenant, and request_id is the
	// unique half of every link row, so the read cannot reach another request's
	// lineage. org_id is restated anyway: it is the tenant the caller named.
	sql := `SELECT ` + requestLinkCols + ` FROM request_links
		WHERE request_id = $1 AND org_id = $2`
	args := []any{requestID, orgID}
	if len(kinds) > 0 {
		names := make([]string, len(kinds))
		for i, k := range kinds {
			names[i] = string(k)
		}
		sql += ` AND link_kind = ANY($3)`
		args = append(args, names)
	}
	rows, err := r.db.Query(ctx, sql+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("links of request %d: %w", requestID, err)
	}
	defer rows.Close()
	out := []domain.RequestLink{}
	for rows.Next() {
		var l domain.RequestLink
		if err := scanRequestLink(rows, &l); err != nil {
			return nil, fmt.Errorf("scan link of request %d: %w", requestID, err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("links of request %d: %w", requestID, err)
	}
	return out, nil
}

// ActiveRequestRoute returns the request's current routing — its newest routes_to
// row — or nil when it is not routed to a project yet.
//
// This is the single definition of "which project" (D9), and it is a different
// question from RequestLinks' "where has it been routed?": a re-route appends a row
// and leaves the previous one in the lineage, so the newest row is the active answer
// and every older row is history. Reads outside this file that report a request's
// project must call this, not "the first routes_to row".
func (r *Repo) ActiveRequestRoute(ctx context.Context, requestID, orgID, teamID int) (*domain.RequestLink, error) {
	if _, err := getRequest(ctx, r.db, requestID, orgID, teamID); err != nil {
		return nil, err
	}
	return activeRequestRoute(ctx, r.db, requestID, orgID)
}

// activeRequestRoute is ActiveRequestRoute's body, taking the one read surface it
// needs so a locked transaction reuses it.
func activeRequestRoute(ctx context.Context, q queryRower, requestID, orgID int) (*domain.RequestLink, error) {
	l := &domain.RequestLink{}
	err := scanRequestLink(q.QueryRow(ctx, `SELECT `+requestLinkCols+` FROM request_links
		WHERE request_id = $1 AND org_id = $2 AND link_kind = $3 ORDER BY id DESC LIMIT 1`,
		requestID, orgID, string(domain.RequestLinkRoutesTo)), l)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("active route of request %d: %w", requestID, err)
	}
	return l, nil
}

// MarkDuplicate folds requestID into canonicalID as a duplicate (D7) and is
// idempotent: folding a request into the canonical it already points at answers
// changed=false and writes nothing.
//
// The act is one transaction: both requests are locked in ascending id order,
// re-read under the lock, the canonical is flattened to the root of its chain (a
// fold that names a duplicate lands on what that duplicate points at), then the
// folded request's duplicate_of and state, its duplicate_of link row, and exactly
// one transition event are written together. A caller that read a stale rev is
// RequestRevConflict; a self-fold, a cycle, an already-folded request and one that
// has duplicates of its own are RequestDuplicateRefused with the rule named.
//
// Concurrency is why the locks are ordered: A→B and B→A at the same time can both
// pass a check-first service, but here the second writer blocks on the shared rows,
// re-reads them, sees the first fold and refuses as a cycle. The result is one
// canonical and one duplicate, never a chain.
//
// A request is folded from whatever non-duplicate state it is in (the event records
// the from state); which states may be folded is the lifecycle service's call, not
// this helper's. closed_at is stamped on this first entry into a terminal state and
// never moved afterwards (053's first-write-wins rule).
func (r *Repo) MarkDuplicate(ctx context.Context, w RequestDuplicateWrite) (*domain.TeamRequest, bool, error) {
	if err := checkRequestWriteScope(w.RequestID, w.OrgID, w.TeamID); err != nil {
		return nil, false, err
	}
	if w.CanonicalID <= 0 {
		return nil, false, fmt.Errorf("request %d: canonical %d: %w", w.RequestID, w.CanonicalID, ErrNotFound)
	}
	if w.CanonicalID == w.RequestID {
		return nil, false, &RequestDuplicateRefused{RequestID: w.RequestID, TargetID: w.CanonicalID, Reason: duplicateRefusedSelf}
	}
	for attempt := 0; attempt < duplicateFoldAttempts; attempt++ {
		req, changed, err := r.foldDuplicate(ctx, w)
		if errors.Is(err, errDuplicateRetry) {
			continue
		}
		return req, changed, err
	}
	return nil, false, &RequestDuplicateRefused{RequestID: w.RequestID, TargetID: w.CanonicalID, Reason: duplicateRefusedChainMoved}
}

// foldDuplicate is one attempt at MarkDuplicate: resolve, lock, re-read, write. It
// answers errDuplicateRetry when the rows it locked are not the rows the act turned
// out to need, which rolls the attempt back and lets MarkDuplicate resolve again.
func (r *Repo) foldDuplicate(ctx context.Context, w RequestDuplicateWrite) (*domain.TeamRequest, bool, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Resolve the named canonical to the root of its chain without a lock, so the
	// lock set can be computed. Everything this read says is verified again below.
	root, err := duplicateRoot(ctx, tx, w.CanonicalID, w.OrgID)
	if err != nil {
		return nil, false, err
	}
	if root == w.RequestID {
		return nil, false, &RequestDuplicateRefused{RequestID: w.RequestID, TargetID: w.CanonicalID, Reason: duplicateRefusedCycle}
	}

	// The request's own duplicates (if any) are part of the lock set, so a concurrent
	// fold into one of them — and a concurrent detach of one — serialises against this
	// fold. The read is advisory: it decides which rows to lock, and the answer is read
	// again under the lock below, because a child folded in between the two reads is a
	// row this set does not hold.
	children, err := duplicateIDsOf(ctx, tx, w.RequestID, w.OrgID)
	if err != nil {
		return nil, false, err
	}
	// plannedChildren is what THIS attempt asked for locks on. It is what lets the
	// re-read below tell the two causes of a child the lock set does not cover apart:
	// a child that appeared after this read (retry), and a child that was in the read
	// but could not be locked because it is in a sibling team (refuse).
	planned := map[int]bool{w.RequestID: true, root: true}
	for _, id := range children {
		planned[id] = true
	}
	lockSet := append([]int{w.RequestID, root}, children...)
	sort.Ints(lockSet)
	lockSet = slices.Compact(lockSet)

	locked, err := lockRequestSet(ctx, tx, lockSet, w.OrgID, w.TeamID)
	if err != nil {
		return nil, false, err
	}
	req, canonical := locked[w.RequestID], locked[root]
	if req == nil {
		return nil, false, fmt.Errorf("request %d: %w", w.RequestID, ErrNotFound)
	}
	if canonical == nil {
		// The canonical moved out of this org (or was deleted) after the resolve. It
		// cannot be folded into, and answering not-found is the package's one 404.
		return nil, false, fmt.Errorf("request %d: %w", w.CanonicalID, ErrNotFound)
	}
	if canonical.DuplicateOf != nil {
		// The named canonical is now itself a duplicate, so the root computed above is
		// stale and the row we would write is not a row we hold. Resolve again.
		return nil, false, errDuplicateRetry
	}
	if req.DuplicateOf != nil {
		if *req.DuplicateOf == root {
			// Already folded into this canonical: a replay, and nothing moves.
			return req, false, nil
		}
		return nil, false, &RequestDuplicateRefused{RequestID: req.ID, TargetID: root, Reason: duplicateRefusedFolded}
	}
	if err := checkRequestExpectRev(req, w.ExpectRev); err != nil {
		return nil, false, err
	}
	// Re-read the children under the lock, and not from the advisory read above. A
	// concurrent fold of X into this request locks this request too, so it either
	// committed before the lock was taken (and this read sees X) or is still waiting
	// (and will re-read this request as a duplicate once this fold commits). The one
	// case the pre-lock read cannot see is exactly the one that would strand a child in
	// a two-hop chain: X folded in between the read and the lock. Every duplicate that
	// points at this request must be a row we hold, or the next attempt has to lock it.
	children, err = duplicateIDsOf(ctx, tx, req.ID, w.OrgID)
	if err != nil {
		return nil, false, err
	}
	for _, id := range children {
		if locked[id] != nil {
			continue
		}
		if !planned[id] {
			// Folded in after the read above: a row this attempt never asked to lock.
			// Resolve again, now that it is visible, so the retry locks it too.
			return nil, false, errDuplicateRetry
		}
		// Asked for and not returned: the child is a 053-legal same-org duplicate that
		// lives in another team, so this tenant cannot lock or move it. Folding anyway
		// would leave it pointing at a request that is now a duplicate — a two-hop
		// chain this write could not flatten. Refuse and say why.
		return nil, false, &RequestDuplicateRefused{RequestID: req.ID, TargetID: root, Reason: duplicateRefusedChildElsewhere}
	}
	// A request that is itself a canonical cannot become a duplicate: its duplicates
	// point at it, and the one-hop invariant ("every duplicate points straight at a
	// non-duplicate canonical") would break. Fold or detach them first — each of
	// those is its own two-row act, so no chain rewrite is hidden inside a fold.
	if len(children) > 0 {
		return nil, false, &RequestDuplicateRefused{RequestID: req.ID, TargetID: root, Reason: duplicateRefusedHasDuplicates}
	}

	from := req.State
	err = scanRequest(tx.QueryRow(ctx, `UPDATE team_requests
		SET state = $2, duplicate_of = $3, closed_at = COALESCE(closed_at, NOW()),
		    rev = rev + 1, updated_at = NOW()
		WHERE id = $1 AND org_id = $4 AND team_id = $5
		RETURNING `+requestCols,
		req.ID, string(domain.RequestStatusDuplicate), root, req.OrgID, req.TeamID), req)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("request %d: %w", req.ID, ErrNotFound)
		}
		return nil, false, fmt.Errorf("fold request %d into %d: %w", req.ID, root, err)
	}

	link := &domain.RequestLink{
		RequestID:  req.ID,
		LinkKind:   domain.RequestLinkDuplicateOf,
		TargetKind: domain.RequestTargetRequest,
		TargetID:   root,
		CreatedBy:  w.CreatedBy,
	}
	if _, err := insertRequestLink(ctx, tx, req, link); err != nil {
		return nil, false, requestLinkUserRefError(err, w.CreatedBy)
	}
	if err := appendRequestLinkEvent(ctx, tx, req, domain.RequestActionTransition, w.Actor, w.ActorUserID,
		from, domain.RequestStatusDuplicate, duplicateEventDetail{
			CanonicalRequestID: root,
			RequestedTargetID:  w.CanonicalID,
		}); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return req, true, nil
}

// DetachDuplicate reopens a folded request: it clears duplicate_of, moves the state
// out of duplicate in the SAME update (053 refuses a duplicate with no canonical),
// records the reopen (reopened_at, reopened_count + 1) and appends one transition
// event. It is the one helper a duplicate reopen needs — not a lifecycle service,
// and not the general state machine (slice S5 owns that).
//
// It is idempotent in the way a release is: a request that is not a duplicate is
// already detached, so the call answers changed=false and writes nothing. A stale
// ExpectRev is RequestRevConflict and nothing moves. ReopenTo is D6's destination
// (triaging or needs_information; empty means triaging); needs_information gets
// needs_info_since stamped so 053's CHECK holds.
//
// The live duplicate_of link row is removed with the pointer — the relation is no
// longer true — while the fold itself stays readable in the request's history
// (request_events is append-only), which is what "don't delete lineage" means here.
func (r *Repo) DetachDuplicate(ctx context.Context, w RequestDuplicateWrite) (*domain.TeamRequest, bool, error) {
	if err := checkRequestWriteScope(w.RequestID, w.OrgID, w.TeamID); err != nil {
		return nil, false, err
	}
	reopenTo, err := normalizeDuplicateReopenState(w.ReopenTo)
	if err != nil {
		return nil, false, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	req, err := lockRequest(ctx, tx, RequestClaimWrite{RequestID: w.RequestID, OrgID: w.OrgID, TeamID: w.TeamID})
	if err != nil {
		return nil, false, err
	}
	if req.DuplicateOf == nil {
		return req, false, nil
	}
	if err := checkRequestExpectRev(req, w.ExpectRev); err != nil {
		return nil, false, err
	}
	foldedInto := *req.DuplicateOf

	// The pointer and the state leave duplicate together, in one statement: 053's
	// CHECK refuses the instant in which one has moved and the other has not.
	err = scanRequest(tx.QueryRow(ctx, `UPDATE team_requests
		SET state = $2, duplicate_of = NULL,
		    reopened_at = COALESCE(reopened_at, NOW()),
		    reopened_count = reopened_count + 1,
		    needs_info_since = CASE WHEN $2 = 'needs_information'
		                           THEN COALESCE(needs_info_since, NOW())
		                           ELSE needs_info_since END,
		    rev = rev + 1, updated_at = NOW()
		WHERE id = $1 AND org_id = $3 AND team_id = $4
		RETURNING `+requestCols,
		req.ID, string(reopenTo), req.OrgID, req.TeamID), req)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("request %d: %w", req.ID, ErrNotFound)
		}
		return nil, false, fmt.Errorf("detach duplicate request %d: %w", req.ID, err)
	}

	var removedLinkID int64
	err = tx.QueryRow(ctx, `DELETE FROM request_links
		WHERE request_id = $1 AND link_kind = $2 AND target_kind = $3 AND target_id = $4 RETURNING id`,
		req.ID, string(domain.RequestLinkDuplicateOf), string(domain.RequestTargetRequest), foldedInto).Scan(&removedLinkID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("remove duplicate link of request %d: %w", req.ID, err)
	}

	if err := appendRequestLinkEvent(ctx, tx, req, domain.RequestActionTransition, w.Actor, w.ActorUserID,
		domain.RequestStatusDuplicate, reopenTo, duplicateEventDetail{
			CanonicalRequestID: foldedInto,
			DetachedLinkID:     removedLinkID,
		}); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return req, true, nil
}

// ---- The pieces the operations above share ----

// validatedRequestLink normalizes and checks one relation: the kind/target pair the
// contract allows (through domain.NormalizeRequestLink, so the schema CHECK and this
// path cannot disagree), a positive target, no self-link, and the one kind the
// general paths do not own.
func validatedRequestLink(w RequestLinkWrite) (*domain.RequestLink, error) {
	if err := checkRequestWriteScope(w.RequestID, w.OrgID, w.TeamID); err != nil {
		return nil, err
	}
	l := &domain.RequestLink{
		RequestID:  w.RequestID,
		LinkKind:   w.LinkKind,
		TargetKind: w.TargetKind,
		TargetID:   w.TargetID,
		CreatedBy:  w.CreatedBy,
	}
	if err := l.Validate(); err != nil {
		return nil, fmt.Errorf("link request %d: %w", w.RequestID, err)
	}
	if l.LinkKind == domain.RequestLinkDuplicateOf {
		return nil, ErrRequestLinkDuplicate
	}
	return l, nil
}

// findRequestLink returns the stored relation, or nil when there is none. It reads
// through requestLinkCols and is scoped by the request's org, so it can only ever
// see rows inside the tenant the caller named.
func findRequestLink(ctx context.Context, q queryRower, requestID, orgID int, kind domain.RequestLinkKind, target domain.RequestTargetKind, targetID int) (*domain.RequestLink, error) {
	l := &domain.RequestLink{}
	err := scanRequestLink(q.QueryRow(ctx, `SELECT `+requestLinkCols+` FROM request_links
		WHERE request_id = $1 AND org_id = $2 AND link_kind = $3 AND target_kind = $4 AND target_id = $5`,
		requestID, orgID, string(kind), string(target), targetID), l)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s link of request %d: %w", kind, requestID, err)
	}
	return l, nil
}

// verifyRequestLinkTarget is the tenant check the schema cannot make, in the two
// steps its own header describes: resolve the target's org from the target's table,
// then compare it with the request's.
//
// The two failures are deliberately different errors. A target whose real org is not
// the request's is a cross-tenant link attempt and answers exactly like a missing
// target (ErrNotFound — one 404, no existence leak). A caller that passed no org, or
// an org the target disagrees with, did not resolve the target from its own record
// (it read metadata, or guessed) and gets ErrRequestLinkTargetIdentity: that is a
// bug in the caller, and answering 404 would hide it behind a legitimate refusal.
func verifyRequestLinkTarget(ctx context.Context, q queryRower, req *domain.TeamRequest, w RequestLinkWrite, l *domain.RequestLink) error {
	if l.TargetKind == domain.RequestTargetRequest && l.TargetID == req.ID {
		// domain.Validate refuses this too; kept here because the two paths that
		// reach this function must both be unable to write a self-link.
		return fmt.Errorf("request %d cannot link to itself", req.ID)
	}
	if w.TargetOrgID <= 0 {
		return fmt.Errorf("link %s %d to request %d: %w", l.TargetKind, l.TargetID, req.ID, ErrRequestLinkTargetIdentity)
	}
	sql, ok := requestLinkTargetOrgQueries[l.TargetKind]
	if !ok {
		// domain.NormalizeRequestLink closed the set; an unknown target kind cannot
		// get here, and a silent pass would be a tenant-check hole.
		return fmt.Errorf("link target %q has no tenant resolution: %w", l.TargetKind, ErrRequestLinkTargetIdentity)
	}
	var targetOrg *int
	err := q.QueryRow(ctx, sql, l.TargetID).Scan(&targetOrg)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && targetOrg == nil) {
		return fmt.Errorf("%s %d: %w", l.TargetKind, l.TargetID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("resolve %s %d's org: %w", l.TargetKind, l.TargetID, err)
	}
	if *targetOrg != w.TargetOrgID {
		return fmt.Errorf("%s %d is in org %d, not the verified org %d the caller supplied: %w",
			l.TargetKind, l.TargetID, *targetOrg, w.TargetOrgID, ErrRequestLinkTargetIdentity)
	}
	if *targetOrg != req.OrgID {
		// Cross-tenant: answer like a target that is not there.
		return fmt.Errorf("%s %d: %w", l.TargetKind, l.TargetID, ErrNotFound)
	}
	return nil
}

// insertRequestLink stores one relation, riding 055's unique key so a replay
// converges to the existing row instead of a second one; created_by/created_at on
// that row are untouched (first-write-wins). It reports whether this call inserted.
func insertRequestLink(ctx context.Context, tx pgx.Tx, req *domain.TeamRequest, l *domain.RequestLink) (bool, error) {
	err := tx.QueryRow(ctx, `INSERT INTO request_links
		(request_id, org_id, link_kind, target_kind, target_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (request_id, target_kind, target_id, link_kind) DO NOTHING
		RETURNING id, created_at`,
		req.ID, req.OrgID, string(l.LinkKind), string(l.TargetKind), l.TargetID, l.CreatedBy,
	).Scan(&l.ID, &l.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("attach %s %d to request %d: %w", l.TargetKind, l.TargetID, req.ID, err)
	}
	return true, nil
}

// requestLinkUserRefError turns a foreign-key refusal on created_by into the ordinary
// not-found for that user. The only foreign key a link insert touches is created_by
// (055: ON DELETE SET NULL), so a 23503 here can mean nothing else, and naming the
// user lets a caller answer "no such user" instead of reporting a constraint
// violation as a server fault.
func requestLinkUserRefError(err error, userID *int) error {
	if userID == nil || !isForeignKeyViolation(err) {
		return err
	}
	return fmt.Errorf("user %d: %w", *userID, ErrNotFound)
}

// duplicateRoot follows a request's duplicate chain to the canonical and returns it.
// Every write in this file flattens, so a stored chain is one hop; the loop and its
// bound exist so a row edited by hand (or written by a path that ran before this
// file) cannot spin.
//
// The walk is scoped by ORG, which is the scope 053 gives the relation:
// team_requests_duplicate_same_tenant is FOREIGN KEY (duplicate_of, org_id), so a
// duplicate's canonical is guaranteed to share its org and nothing guarantees it
// shares its team. Reading the chain more narrowly would not see a canonical in a
// sibling team and would report a request as un-folded while it is folded. Whether
// the chain's far end is inside the CALLER's tenant is a separate question, answered
// when the fold goes to lock it.
func duplicateRoot(ctx context.Context, q queryRower, requestID, orgID int) (int, error) {
	current := requestID
	for hop := 0; hop < maxDuplicateChain; hop++ {
		var next *int
		err := q.QueryRow(ctx, `SELECT duplicate_of FROM team_requests
			WHERE id = $1 AND org_id = $2`, current, orgID).Scan(&next)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("request %d: %w", current, ErrNotFound)
		}
		if err != nil {
			return 0, fmt.Errorf("resolve duplicate chain of request %d: %w", requestID, err)
		}
		if next == nil {
			return current, nil
		}
		if *next == current {
			// 053's CHECK refuses a self-duplicate; a row that has one is corrupt, and
			// folding against it would write a cycle.
			return 0, fmt.Errorf("request %d is its own duplicate: %w", current, ErrConflict)
		}
		current = *next
	}
	return 0, fmt.Errorf("request %d's duplicate chain is longer than %d hops: %w", requestID, maxDuplicateChain, ErrConflict)
}

// duplicateIDsOf lists the requests folded into requestID, ascending. Used to prove
// the request is not itself a canonical and to know which rows a fold must lock.
//
// Org-scoped for the same reason duplicateRoot is: 053's composite key makes
// duplicate_of a same-org relation, so a child in a sibling team is a real child this
// read must see. Seeing it is what lets the fold refuse (the row is outside the
// caller's tenant, so it cannot be locked or moved) instead of writing a fold that
// leaves a chain behind.
func duplicateIDsOf(ctx context.Context, q querier, requestID, orgID int) ([]int, error) {
	rows, err := q.Query(ctx, `SELECT id FROM team_requests
		WHERE duplicate_of = $1 AND org_id = $2 ORDER BY id`, requestID, orgID)
	if err != nil {
		return nil, fmt.Errorf("duplicates of request %d: %w", requestID, err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("duplicates of request %d: %w", requestID, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("duplicates of request %d: %w", requestID, err)
	}
	return out, nil
}

// requestLinkEventDetail is what a link or unlink event explains: which row, which
// relation, to what, and — for a re-route — which project it superseded. Ids only:
// never a name, an email or a title, because a request's history is
// requester-visible and an id the reader may not resolve is a smaller leak than a
// display name they may not see (the claim file's rule).
type linkEventDetail struct {
	LinkID              int64  `json:"link_id"`
	LinkKind            string `json:"link_kind"`
	TargetKind          string `json:"target_kind"`
	TargetID            int    `json:"target_id"`
	SupersededProjectID *int   `json:"superseded_project_id,omitempty"`
}

// duplicateEventDetail is what a fold or a detach explains: which request is the
// canonical, and — on a detach — which link row stopped being true. A fold names the
// canonical the caller asked for too, because "B was named, C was the canonical" is
// the flattening a reader has to be able to see.
type duplicateEventDetail struct {
	CanonicalRequestID int   `json:"canonical_request_id"`
	RequestedTargetID  int   `json:"requested_target_id,omitempty"`
	DetachedLinkID     int64 `json:"detached_link_id,omitempty"`
}

// appendRequestLinkEvent appends the one history row an act gets (I6), in the
// transaction that made the change, and supports the two transitions a fold and a
// detach are: from/to are empty for a link or unlink and set for a transition (054's
// CHECK enforces exactly that).
func appendRequestLinkEvent(ctx context.Context, tx pgx.Tx, req *domain.TeamRequest, action domain.RequestEventAction,
	actor string, actorUserID *int, from, to domain.RequestStatus, detail any) error {
	ev := domain.RequestEvent{
		RequestID:   req.ID,
		OrgID:       req.OrgID,
		TeamID:      req.TeamID,
		Action:      action,
		FromStatus:  from,
		ToStatus:    to,
		Actor:       actor,
		ActorUserID: actorUserID,
	}
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("record %s event for request %d: %w", action, req.ID, err)
	}
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO request_events
		(request_id, org_id, team_id, action, from_state, to_state, actor, actor_user_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		ev.RequestID, ev.OrgID, ev.TeamID, string(ev.Action), string(ev.FromStatus), string(ev.ToStatus),
		ev.Actor, ev.ActorUserID, requestEventDetail(detail),
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("record %s event for request %d: %w", action, req.ID, err)
	}
	return nil
}

// normalizeDuplicateReopenState validates D6's destination for a detach: triaging
// (the default) or needs_information. Anything else — including delivered or
// requester_accepted — is not a reopen, it is a lifecycle move, and slice S5 owns
// those; answering so here keeps one helper from becoming a second state machine.
func normalizeDuplicateReopenState(s domain.RequestStatus) (domain.RequestStatus, error) {
	if s == "" {
		return domain.RequestStatusTriaging, nil
	}
	st, err := domain.NormalizeRequestStatus(string(s))
	if err != nil {
		return "", err
	}
	switch st {
	case domain.RequestStatusTriaging, domain.RequestStatusNeedsInformation:
		return st, nil
	default:
		return "", fmt.Errorf("a duplicate reopens to triaging or needs_information (D6), not %s", st)
	}
}

// lockRequestSet locks every request in ids (all in orgID) in ascending id order and
// returns them by id. One ordered statement is what makes the locks deadlock-free
// between two concurrent folds: every writer acquires the shared rows in the same
// order, and the case the ordering exists for — two triagers naming either end of
// the same pair — is the normal one rather than a corner.
func lockRequestSet(ctx context.Context, tx pgx.Tx, ids []int, orgID, teamID int) (map[int]*domain.TeamRequest, error) {
	if len(ids) == 0 {
		return map[int]*domain.TeamRequest{}, nil
	}
	// ids arrive sorted (slices.Compact of a sort.Ints), and ORDER BY id restates the
	// order the locks are taken in rather than relying on the caller's sort. Both
	// halves of the tenant are in the predicate: a fold writes team_requests rows, and
	// a write scoped by org alone would let a caller authorized for one team fold a
	// request in another whose row its own tenant never named.
	rows, err := tx.Query(ctx, requestSelect+` WHERE id = ANY($1) AND org_id = $2 AND team_id = $3 ORDER BY id FOR UPDATE`,
		int32s(ids), orgID, teamID)
	if err != nil {
		return nil, fmt.Errorf("lock requests %v: %w", ids, err)
	}
	defer rows.Close()
	out := make(map[int]*domain.TeamRequest, len(ids))
	for rows.Next() {
		req := &domain.TeamRequest{}
		if err := scanRequest(rows, req); err != nil {
			return nil, fmt.Errorf("scan locked request: %w", err)
		}
		out[req.ID] = req
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock requests %v: %w", ids, err)
	}
	return out, nil
}

// int32s converts ids for a Postgres int[] argument. pgx maps []int32, and an empty
// slice stays empty rather than becoming NULL (which ANY would treat as no rows).
func int32s(ids []int) []int32 {
	out := make([]int32, len(ids))
	for i, id := range ids {
		out[i] = int32(id)
	}
	return out
}
