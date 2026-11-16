package repository

// Team requests: the durable half of the planning-handoff ledger (epic 202, story
// 549, AMP #3965; approved product contract artifact 36 v2 §3 and default D11 —
// "reconcile-on-retry, no batch-transaction rewrite"; build slice E1's handoff half
// in discoveries/team-inbox-approved-build-slices.md).
//
// WHY THIS IS A READ AND NOT A TABLE. The obvious ledger is a `request_handoffs`
// table with one row per (handoff, ref, task) written after the batch. It has a
// crash window: Service.CreateTasks is not transactional by design (D11), so
// between the ticket INSERT and the ledger INSERT there is a moment where a ticket
// exists and nothing records it. A process that dies there loses the mapping, and
// the retry — which is the whole point of the retry — duplicates the ticket.
//
// The ledger is therefore carried ON the ticket: domain.MetaRequestHandoff holds
// "<request id>/<handoff key>/<batch ref>" in the ticket's own metadata, written by
// the same INSERT that creates the ticket (CreateTaskRequest.Metadata →
// tasks.metadata). The row that proves the batch item was created IS the ticket, so
// there is no window: either both are there or neither is. This file is the read
// that turns those labels back into the ref→id mapping a retry reconciles against.
//
// It is a label and not the relation, deliberately. Which request a ticket serves
// is its request_links planned_as row — typed, verified, tombstoned on delete, and
// what the delivery projection reads. The marker only answers "which item of which
// handoff made this ticket", a question whose only reader is a retry of that
// handoff. Losing it degrades a retry to a duplicate warning; it never loses
// lineage, which is the contract's boundary between a relation and a label
// (discoveries/team-request-planning-lineage.md: "a metadata key must never be the
// only place a link, ACL or lifecycle guarantee lives").
//
// The match is exact, not a LIKE. handoff keys and refs may contain "_", which is a
// LIKE wildcard, so a prefix match would need escaping and would still be a prefix.
// split_part on the marker's own separator answers "same request, same handoff"
// exactly, which is the only question worth asking here.

import (
	"context"
	"fmt"

	"github.com/simstech/amp-api/internal/domain"
)

// RequestHandoffRef is one batch item an earlier attempt of this handoff already
// created: the caller's ref and the id of the ticket it became.
type RequestHandoffRef struct {
	Ref    string `json:"ref"`
	TaskID int    `json:"task_id"`
}

// RequestHandoffLedger returns the batch items an earlier attempt of handoffKey for
// request requestID already created, so a retried handoff can plan only the rest.
//
// The search is bounded twice over, and both bounds are needed:
//
//   - projectID, the request's active route, is where this handoff plans. It is the
//     indexed half of the predicate, and it is why this is not a scan of every
//     ticket in the installation.
//   - the request's own planned_as task links cover the one case the route cannot:
//     a request re-routed between two attempts. A ticket an earlier attempt made is
//     linked to the request, and a link outlives a re-route, so the request's own
//     lineage finds it even though the route no longer points at its project.
//
// A malformed marker is not an error: domain.ParseRequestHandoffMark answers "not a
// handoff ticket" for anything this package did not write, so one hand-edited row
// cannot break a retry. A marker that resolves to a ref this handoff already saw on
// a different ticket IS an error — two tickets claiming one batch item is not a
// state a retry can reconcile, and guessing which one is real would duplicate or
// orphan work.
func (r *Repo) RequestHandoffLedger(ctx context.Context, requestID, orgID, teamID, projectID int, handoffKey string) ([]RequestHandoffRef, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("handoff ledger of request %d: a project is required (a handoff plans into the request's routed project)", requestID)
	}
	key, err := domain.NormalizeHandoffKey(handoffKey)
	if err != nil {
		return nil, fmt.Errorf("handoff ledger: %w", err)
	}

	// The request's own plan, read through the tenant-checked path so this cannot
	// become a cross-tenant read primitive. A failure is a real failure: answering
	// "no links" would turn a database problem into duplicated tickets on the
	// retry, which is the one outcome this file exists to prevent.
	links, err := r.RequestLinks(ctx, requestID, orgID, teamID, domain.RequestLinkPlannedAs)
	if err != nil {
		return nil, fmt.Errorf("handoff ledger of request %d: %w", requestID, err)
	}
	linked := make([]int32, 0, len(links))
	for _, l := range links {
		if l.TargetKind == domain.RequestTargetTask {
			linked = append(linked, int32(l.TargetID))
		}
	}

	// The marker's first two parts are compared as text (split_part), never as a
	// pattern: the request id is a number and the key is validated above, so the
	// comparison is exact and there is no escaping to get wrong.
	rows, err := r.db.Query(ctx, `
		SELECT id, metadata->>'`+domain.MetaRequestHandoff+`'
		  FROM tasks
		 WHERE split_part(metadata->>'`+domain.MetaRequestHandoff+`', '/', 1) = $1
		   AND split_part(metadata->>'`+domain.MetaRequestHandoff+`', '/', 2) = $2
		   AND (project_id = $3 OR id = ANY($4))
		 ORDER BY id`,
		fmt.Sprint(requestID), key, projectID, linked)
	if err != nil {
		return nil, fmt.Errorf("handoff ledger of request %d: %w", requestID, err)
	}
	defer rows.Close()

	out := make([]RequestHandoffRef, 0, 4)
	seen := map[string]int{}
	for rows.Next() {
		var id int
		var mark string
		if err := rows.Scan(&id, &mark); err != nil {
			return nil, fmt.Errorf("handoff ledger of request %d: %w", requestID, err)
		}
		gotRequest, gotKey, ref, ok := domain.ParseRequestHandoffMark(mark)
		if !ok || gotRequest != requestID || gotKey != key {
			continue
		}
		if prev, dup := seen[ref]; dup {
			return nil, fmt.Errorf("handoff ledger of request %d: ref %q is claimed by ticket %d and ticket %d; "+
				"two tickets cannot be the same batch item", requestID, ref, prev, id)
		}
		seen[ref] = id
		out = append(out, RequestHandoffRef{Ref: ref, TaskID: id})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("handoff ledger of request %d: %w", requestID, err)
	}
	return out, nil
}
