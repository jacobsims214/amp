package repository

// Team requests: the typed facts the outcome aggregate reads (epic 202, story
// 549, AMP #3966; build slice E2 of
// discoveries/team-inbox-approved-build-slices.md).
//
// The aggregate itself (service/request_outcome.go) is pure: it decides nothing
// from a database, an actor or a clock, so every case it has to get right is a
// table test. That means the facts it needs must arrive as typed values, and
// this file is the one place that reads them — all of them tenant-scoped through
// the request's own (org, team), all of them batched, and none of them a join
// across a tenant boundary.
//
// What it reads, and why each read is separate:
//
//   - the requests themselves (the batch loader), so one page of a queue can be
//     aggregated without a query per row;
//   - the delivery links (request_links, already the durable relation, never
//     metadata: contract §3), read by kind because an aggregate that counted a
//     planned_as row as delivery evidence would be the exact
//     "something is attached" claim I2 forbids;
//   - the links' TARGETS as they are now — a ticket with its state, phase inputs
//     and merge state; an artifact with its review state; a receipt with its
//     held flag — because a link outlives its target and the aggregate must
//     report what is there rather than what was there when it was linked;
//   - the tombstones (deleted_tasks) for targets that are gone, so a deleted
//     ticket is "re-planned", not silence (D10's whole point);
//   - the tickets' runs, because closeouts and abandoned runs are rework and an
//     outcome that says "5 of 5 done" without them overstates what happened.
//
// Nothing here decides: a target in another org, a receipt whose ticket is gone,
// a run with no dispatch id — each is returned as it is, and the aggregate is
// where the meaning of it lives.

import (
	"context"
	"errors"
	"fmt"

	"github.com/simstech/amp-api/internal/domain"
)

// RequestOutcomeLinkTarget is one link target resolved from its own table, as
// the aggregate needs it: what kind of thing it is, its id, whether it still
// exists at all, and — when it does — the fields that say what state it is in.
//
// It is deliberately NOT *domain.Task / *domain.Artifact / *domain.Receipt:
// those types carry names, descriptions, assignees, branches, owners and
// metadata that the requester projection must never see, and handing the whole
// record to the aggregate would make it possible for one to leak through a note
// later. Only the fields that answer "is this done, gated, held or verified"
// are here.
type RequestOutcomeLinkTarget struct {
	Kind domain.RequestTargetKind
	ID   int

	// Deleted is set when the target's own table has no row any more, and
	// DeletedAt is when its tombstone says it went (nil if the tombstone is
	// not readable). A deleted target is a re-planned slice, never silence.
	Deleted   bool
	DeletedAt *string

	// --- task ---
	TaskState   domain.TaskState
	TaskStarted bool   // started_at is set: the started/not-started phase
	TaskBlocked string // block_reason, for the question-vs-dependency distinction
	MergeState  string // domain.MergeNone / MergeNeeded / MergeMerged / MergeAbandoned
	MergeGated  bool   // domain.MergeGates(MergeState): done, still holding dependents

	// --- artifact ---
	ArtifactStatus   string // in_review / approved / changes_requested
	ArtifactApproved bool   // approved_version is set and equals current_version

	// --- receipt ---
	ReceiptState   string // draft / final
	ReceiptHeld    bool   // held for a person (a Require-mode completion)
	ReceiptAgentID string // whose run it is: DeriveStatus needs it
	// ReceiptChecks are the receipt's checks. They are here so the aggregate can
	// call domain.DeriveStatus — the one definition of a receipt's verification
	// status (receipt.go) — rather than growing a second ladder in a request file.
	ReceiptChecks []domain.ReceiptCheck
}

// RequestOutcomeRunFacts is what the aggregate reads of one ticket's runs: how
// many there are, how many were abandoned, and whether any of them carried work
// to a merge gate. Tokens and costs are not here on purpose — the aggregate is
// about honesty of state, not spend (Usage is the spend read).
type RequestOutcomeRunFacts struct {
	TaskID    int
	Runs      int
	Abandoned int // runs that died or were stopped: rework
}

// RequestOutcomeFacts is everything one request's aggregate is computed from:
// the stored row, its links by kind, its targets as they are now, and its
// tickets' runs. Every field is read tenant-scoped for this request.
type RequestOutcomeFacts struct {
	Request *domain.TeamRequest

	// Links is the request's whole lineage (every kind), so the aggregate can
	// tell a request that was planned from one that was routed and never
	// planned, and can see duplicates without a second read.
	Links []domain.RequestLink
	// Targets is one entry per distinct (kind, id) among Links.
	Targets []RequestOutcomeLinkTarget
	// Runs is one entry per distinct task target id.
	Runs []RequestOutcomeRunFacts
	// NoArtifact records that the delivery said the outcome exists with nothing
	// to link. 053 stores no column for it; the delivery's own history row says
	// so (see deliveredWithoutArtifact in the service), so this read computes it
	// from request_events rather than inventing a column.
	NoArtifact bool
}

// RequestOutcomeFactsFor reads one request's aggregate inputs, tenant-scoped.
// It returns repository.ErrNotFound for a request in another tenant, exactly as
// every other request read does, so a caller cannot tell "not yours" from "not
// there".
func (r *Repo) RequestOutcomeFactsFor(ctx context.Context, requestID, orgID, teamID int) (*RequestOutcomeFacts, error) {
	req, err := getRequest(ctx, r.db, requestID, orgID, teamID)
	if err != nil {
		return nil, err
	}
	facts := &RequestOutcomeFacts{Request: req}
	if facts.Links, err = r.RequestLinks(ctx, requestID, orgID, teamID); err != nil {
		return nil, err
	}
	if facts.Targets, err = r.requestOutcomeTargets(ctx, facts.Links); err != nil {
		return nil, err
	}
	if facts.Runs, err = r.requestOutcomeRuns(ctx, facts.Targets); err != nil {
		return nil, err
	}
	if facts.NoArtifact, err = r.requestDeliveredWithoutArtifact(ctx, requestID, orgID, teamID); err != nil {
		return nil, err
	}
	return facts, nil
}

// RequestOutcomeFactsForRequests is the batch form the queue and list reads use:
// one aggregate's inputs per request id, in one round trip per fact kind rather
// than one per row. Ids in another tenant are simply absent from the map (a
// list is filtered by the caller's tenant before it gets here), never an error.
func (r *Repo) RequestOutcomeFactsForRequests(ctx context.Context, orgID, teamID int, requestIDs []int) (map[int]*RequestOutcomeFacts, error) {
	out := make(map[int]*RequestOutcomeFacts, len(requestIDs))
	if len(requestIDs) == 0 {
		return out, nil
	}
	reqs, err := r.getRequests(ctx, requestIDs, orgID, teamID)
	if err != nil {
		return nil, err
	}
	linksByReq := make(map[int][]domain.RequestLink, len(requestIDs))
	all := make([]domain.RequestLink, 0, len(requestIDs))
	for id := range reqs {
		links, err := r.RequestLinks(ctx, id, orgID, teamID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				delete(reqs, id)
				continue
			}
			return nil, err
		}
		linksByReq[id] = links
		all = append(all, links...)
	}
	targets, err := r.requestOutcomeTargets(ctx, all)
	if err != nil {
		return nil, err
	}
	runs, err := r.requestOutcomeRuns(ctx, targets)
	if err != nil {
		return nil, err
	}
	noArtifact, err := r.requestDeliveredWithoutArtifactBatch(ctx, orgID, teamID, requestIDs)
	if err != nil {
		return nil, err
	}
	// Targets are shared across requests (one ticket may serve two requests), so
	// each request gets only its own distinct targets while the reads stay
	// batched over the union.
	byKey := make(map[string]RequestOutcomeLinkTarget, len(targets))
	for _, t := range targets {
		byKey[targetKey(t.Kind, t.ID)] = t
	}
	for id, req := range reqs {
		facts := &RequestOutcomeFacts{Request: req, Links: linksByReq[id], NoArtifact: noArtifact[id]}
		seen := map[string]bool{}
		seenTask := map[int]bool{}
		for _, l := range facts.Links {
			key := targetKey(l.TargetKind, l.TargetID)
			if !seen[key] {
				if t, ok := byKey[key]; ok {
					facts.Targets = append(facts.Targets, t)
				}
				seen[key] = true
			}
			if l.TargetKind == domain.RequestTargetTask && !seenTask[l.TargetID] {
				seenTask[l.TargetID] = true
				for _, rf := range runs {
					if rf.TaskID == l.TargetID {
						facts.Runs = append(facts.Runs, rf)
					}
				}
			}
		}
		out[id] = facts
	}
	return out, nil
}

// targetKey is the aggregate's target identity: the pair a link is unique on.
func targetKey(kind domain.RequestTargetKind, id int) string {
	return fmt.Sprintf("%s:%d", kind, id)
}

// requestOutcomeTargets resolves each distinct link target from its own table.
//
// One statement per target kind, each batched over the ids in this tenant's
// links, because a target kind's "is it done" lives in its own row and a union
// would have to invent a common shape. A target with no row is marked Deleted
// and its tombstone is looked up in the same read (deleted_tasks keeps a
// snapshot for 30 days; after that the deletion is still reported, only without
// a date, which is honest).
//
// Tenant safety: a link's target was verified to be in the request's org when it
// was written (LinkRequest), and this read does not widen it — a task in another
// org would be returned as a target with no row, i.e. deleted, rather than with
// its foreign state.
func (r *Repo) requestOutcomeTargets(ctx context.Context, links []domain.RequestLink) ([]RequestOutcomeLinkTarget, error) {
	if len(links) == 0 {
		return nil, nil
	}
	idsByKind := map[domain.RequestTargetKind][]int{}
	seen := map[string]bool{}
	for _, l := range links {
		key := targetKey(l.TargetKind, l.TargetID)
		if seen[key] {
			continue
		}
		seen[key] = true
		idsByKind[l.TargetKind] = append(idsByKind[l.TargetKind], l.TargetID)
	}

	var out []RequestOutcomeLinkTarget
	if ids := idsByKind[domain.RequestTargetTask]; len(ids) > 0 {
		tasks, err := r.outcomeTasks(ctx, ids)
		if err != nil {
			return nil, err
		}
		out = append(out, tasks...)
	}
	if ids := idsByKind[domain.RequestTargetArtifact]; len(ids) > 0 {
		artifacts, err := r.outcomeArtifacts(ctx, ids)
		if err != nil {
			return nil, err
		}
		out = append(out, artifacts...)
	}
	if ids := idsByKind[domain.RequestTargetReceipt]; len(ids) > 0 {
		receipts, err := r.outcomeReceipts(ctx, ids)
		if err != nil {
			return nil, err
		}
		out = append(out, receipts...)
	}
	// The remaining kinds (project/epic/story/request) do not deliver anything:
	// they are lineage, and the aggregate counts them as planned/routed only.
	return out, nil
}

// outcomeTasks reads the state of each task target, with its merge state, and
// marks the ones with no row as deleted (by tombstone).
func (r *Repo) outcomeTasks(ctx context.Context, ids []int) ([]RequestOutcomeLinkTarget, error) {
	rows, err := r.db.Query(ctx, `SELECT id, state, started_at IS NOT NULL, COALESCE(block_reason, ''),
			COALESCE(merge_state, 'none')
		FROM tasks WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("read task targets: %w", err)
	}
	found := map[int]RequestOutcomeLinkTarget{}
	for rows.Next() {
		t := RequestOutcomeLinkTarget{Kind: domain.RequestTargetTask}
		if err := rows.Scan(&t.ID, &t.TaskState, &t.TaskStarted, &t.TaskBlocked, &t.MergeState); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan task target: %w", err)
		}
		t.MergeGated = domain.MergeGates(t.MergeState)
		found[t.ID] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read task targets: %w", err)
	}
	return r.finishTargets(ctx, domain.RequestTargetTask, ids, found, func(sql []int) (map[int]*string, error) {
		return r.deletedTaskTimes(ctx, sql)
	})
}

// outcomeArtifacts reads each artifact target's review state. Approved means the
// artifact has an approved version and it is the current one: a later version
// that has not been approved is a change in flight, and calling it approved
// would be the "approved" claim D10 wants the aggregate to earn.
func (r *Repo) outcomeArtifacts(ctx context.Context, ids []int) ([]RequestOutcomeLinkTarget, error) {
	rows, err := r.db.Query(ctx, `SELECT id, status, approved_version, current_version
		FROM artifacts WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("read artifact targets: %w", err)
	}
	found := map[int]RequestOutcomeLinkTarget{}
	for rows.Next() {
		t := RequestOutcomeLinkTarget{Kind: domain.RequestTargetArtifact}
		var approved, current *int
		if err := rows.Scan(&t.ID, &t.ArtifactStatus, &approved, &current); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan artifact target: %w", err)
		}
		t.ArtifactApproved = approved != nil && current != nil && *approved >= *current
		found[t.ID] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read artifact targets: %w", err)
	}
	// Artifacts have no tombstone table: an artifact is deleted by expiry, and
	// its row is gone. A link to a gone artifact is still a gone target.
	return r.finishTargets(ctx, domain.RequestTargetArtifact, ids, found, nil)
}

// outcomeReceipts reads each receipt target's state and hold flag. A receipt is
// the evidence of a verification, so "held" is the fact the aggregate reports as
// pending verification rather than delivered.
func (r *Repo) outcomeReceipts(ctx context.Context, ids []int) ([]RequestOutcomeLinkTarget, error) {
	rows, err := r.db.Query(ctx, `SELECT id, state, held, agent_id FROM task_receipts WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("read receipt targets: %w", err)
	}
	found := map[int]RequestOutcomeLinkTarget{}
	var foundIDs []int64
	for rows.Next() {
		t := RequestOutcomeLinkTarget{Kind: domain.RequestTargetReceipt}
		if err := rows.Scan(&t.ID, &t.ReceiptState, &t.ReceiptHeld, &t.ReceiptAgentID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan receipt target: %w", err)
		}
		found[t.ID] = t
		foundIDs = append(foundIDs, int64(t.ID))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read receipt targets: %w", err)
	}
	// The checks are what the verification status is derived from, so a receipt
	// target without them would be a receipt the aggregate could only guess at.
	checks, err := r.ReceiptChecks(ctx, foundIDs)
	if err != nil {
		return nil, err
	}
	for id, t := range found {
		t.ReceiptChecks = checks[int64(id)]
		if t.ReceiptChecks == nil {
			t.ReceiptChecks = []domain.ReceiptCheck{}
		}
		found[id] = t
	}
	return r.finishTargets(ctx, domain.RequestTargetReceipt, ids, found, nil)
}

// finishTargets returns one target per id in the order the ids were given: the
// found ones as read, and every missing one marked deleted with its tombstone
// date when the kind has one.
func (r *Repo) finishTargets(ctx context.Context, kind domain.RequestTargetKind, ids []int,
	found map[int]RequestOutcomeLinkTarget, tombstones func([]int) (map[int]*string, error)) ([]RequestOutcomeLinkTarget, error) {
	var missing []int
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	var gone map[int]*string
	if len(missing) > 0 && tombstones != nil {
		var err error
		if gone, err = tombstones(missing); err != nil {
			return nil, err
		}
	}
	out := make([]RequestOutcomeLinkTarget, 0, len(ids))
	for _, id := range ids {
		if t, ok := found[id]; ok {
			out = append(out, t)
			continue
		}
		t := RequestOutcomeLinkTarget{Kind: kind, ID: id, Deleted: true}
		if at, ok := gone[id]; ok {
			t.DeletedAt = at
		}
		out = append(out, t)
	}
	return out, nil
}

// deletedTaskTimes reads the tombstones of deleted tickets: when each went.
// deleted_tasks keeps a row for 30 days, so a target deleted longer ago is
// reported as deleted without a date rather than as existing.
func (r *Repo) deletedTaskTimes(ctx context.Context, ids []int) (map[int]*string, error) {
	rows, err := r.db.Query(ctx, `SELECT task_id, deleted_at::text FROM deleted_tasks WHERE task_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("read deleted task tombstones: %w", err)
	}
	defer rows.Close()
	out := map[int]*string{}
	for rows.Next() {
		var id int
		var at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, fmt.Errorf("scan deleted task tombstone: %w", err)
		}
		out[id] = &at
	}
	return out, rows.Err()
}

// requestOutcomeRuns reads each task target's run facts: how many, how many
// abandoned, whether any run carried work to a merge, and the latest outcome.
// It is one query over task_runs for the target ids (task_runs has no FK on
// purpose, so a deleted ticket's runs are still here — and the aggregate reads
// them for a deleted target too, because "it ran, then the plan changed" is
// exactly the rework a tombstone should show).
func (r *Repo) requestOutcomeRuns(ctx context.Context, targets []RequestOutcomeLinkTarget) ([]RequestOutcomeRunFacts, error) {
	var ids []int
	for _, t := range targets {
		if t.Kind == domain.RequestTargetTask {
			ids = append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, `SELECT task_id, count(*), count(*) FILTER (WHERE outcome = $2)
		FROM task_runs WHERE task_id = ANY($1) GROUP BY task_id`, ids, domain.RunAbandoned)
	if err != nil {
		return nil, fmt.Errorf("read outcome runs: %w", err)
	}
	defer rows.Close()
	byTask := map[int]*RequestOutcomeRunFacts{}
	for rows.Next() {
		var taskID, runs, abandoned int
		if err := rows.Scan(&taskID, &runs, &abandoned); err != nil {
			return nil, fmt.Errorf("scan outcome run: %w", err)
		}
		byTask[taskID] = &RequestOutcomeRunFacts{TaskID: taskID, Runs: runs, Abandoned: abandoned}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read outcome runs: %w", err)
	}
	out := make([]RequestOutcomeRunFacts, 0, len(byTask))
	for _, t := range targets {
		if f, ok := byTask[t.ID]; ok && t.Kind == domain.RequestTargetTask {
			out = append(out, *f)
		}
	}
	return out, nil
}

// ---- The explicit no-artifact record ----
//
// I2 lets a delivery say "the outcome exists and there is nothing to link". 053
// has no column for it (a delivery that needed no link still sets delivered_at
// and the brief), so the fact is recorded where the act is recorded: the
// transition's history row. Documenting it here rather than adding a column keeps
// the schema as approved; if a later slice wants it typed, this is the one place
// that has to change.
//
// deliveredWithoutArtifact asks whether the request's delivered transition was
// the no-artifact kind. That is knowable from the links: a no-artifact delivery
// is exactly a delivery that had no delivered_by link at the time, and since a
// delivered_by link written later is evidence the outcome has something to point
// at, a request with zero delivered_by links that is delivered (or accepted) was
// declared no-artifact. The read therefore needs no event detail at all — it is
// the same fact the aggregate's own link count states, which is also why the two
// can never disagree.

// requestDeliveredWithoutArtifact answers the no-artifact question for one
// request, tenant-scoped (the request is proved to exist first, so a foreign id
// is ErrNotFound rather than "nothing was declared").
func (r *Repo) requestDeliveredWithoutArtifact(ctx context.Context, requestID, orgID, teamID int) (bool, error) {
	if _, err := getRequest(ctx, r.db, requestID, orgID, teamID); err != nil {
		return false, err
	}
	var delivered bool
	if err := r.db.QueryRow(ctx, `SELECT state IN ('delivered', 'requester_accepted') FROM team_requests
		WHERE id = $1 AND org_id = $2 AND team_id = $3`, requestID, orgID, teamID).Scan(&delivered); err != nil {
		return false, fmt.Errorf("read request %d delivery state: %w", requestID, err)
	}
	if !delivered {
		// Before delivery there is nothing to declare: the no-artifact record is
		// part of the delivery act (I2), so an in-progress request's answer is
		// "not applicable", which the aggregate reads as false.
		return false, nil
	}
	var links int
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM request_links
		WHERE request_id = $1 AND org_id = $2 AND link_kind = 'delivered_by'`, requestID, orgID).Scan(&links); err != nil {
		return false, fmt.Errorf("count delivered links of request %d: %w", requestID, err)
	}
	return links == 0, nil
}

// requestDeliveredWithoutArtifactBatch is the same answer for a page, in two
// queries: the delivered states and the delivered_by counts, joined in memory.
func (r *Repo) requestDeliveredWithoutArtifactBatch(ctx context.Context, orgID, teamID int, requestIDs []int) (map[int]bool, error) {
	out := map[int]bool{}
	if len(requestIDs) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `SELECT id FROM team_requests
		WHERE id = ANY($1) AND org_id = $2 AND team_id = $3 AND state IN ('delivered', 'requester_accepted')`,
		requestIDs, orgID, teamID)
	if err != nil {
		return nil, fmt.Errorf("read delivered requests: %w", err)
	}
	var delivered []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan delivered request: %w", err)
		}
		delivered = append(delivered, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read delivered requests: %w", err)
	}
	if len(delivered) == 0 {
		return out, nil
	}
	counts, err := r.db.Query(ctx, `SELECT request_id, count(*) FROM request_links
		WHERE request_id = ANY($1) AND org_id = $2 AND link_kind = 'delivered_by'
		GROUP BY request_id`, delivered, orgID)
	if err != nil {
		return nil, fmt.Errorf("count delivered links: %w", err)
	}
	defer counts.Close()
	for counts.Next() {
		var id, n int
		if err := counts.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("scan delivered link count: %w", err)
		}
		if n == 0 {
			out[id] = true
		}
	}
	return out, counts.Err()
}

// getRequests loads the requests among ids that are in this tenant's team. It is
// the batch sibling of getRequest (request_rows.go) and reuses that file's
// scan/column contract rather than restating it.
func (r *Repo) getRequests(ctx context.Context, ids []int, orgID, teamID int) (map[int]*domain.TeamRequest, error) {
	out := make(map[int]*domain.TeamRequest, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `SELECT `+requestCols+` FROM team_requests
		WHERE id = ANY($1) AND org_id = $2 AND team_id = $3`, ids, orgID, teamID)
	if err != nil {
		return nil, fmt.Errorf("read requests %v: %w", ids, err)
	}
	defer rows.Close()
	for rows.Next() {
		req := &domain.TeamRequest{}
		if err := scanRequest(rows, req); err != nil {
			return nil, fmt.Errorf("scan request: %w", err)
		}
		out[req.ID] = req
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read requests: %w", err)
	}
	return out, nil
}
