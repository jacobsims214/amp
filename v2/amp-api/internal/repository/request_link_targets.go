package repository

// Team requests: resolving a link target from its OWN record, for the authorized
// attach path (epic 202, story 549, AMP #3962; build slice E1 of
// discoveries/team-inbox-approved-build-slices.md; approved product contract
// artifact 36 v2 §3, defaults D9 and D10).
//
// 055 deliberately gives a link no foreign key on target_id, because a link
// outlives the ticket, artifact or receipt it names (a tombstone). That means
// "the target is in this request's tenant" cannot be a constraint and has to be
// decided by the link service — the schema's own header says so ("tenant safety
// moves to the link service"), and request_links.go enforces it by verifying the
// org value the caller hands it against the target's table.
//
// To do that a service has to answer two questions about a target before it may
// write anything, and both answers come from the target's own row and the project
// it lives in — never from metadata, which is inherited and patchable:
//
//   - which ORG is the target really in (LinkRequest verifies the value it is
//     handed against this same join, and refuses a caller that guessed one);
//   - which PROJECT does the target live in, which the write path does not ask at
//     all — whether the caller may link work at all is a service decision, and a
//     service cannot check access to a project it has not resolved.
//
// This file is that read, and the only place that joins all seven target kinds up
// to their project. A new target kind has one statement to add here, next to the
// one request_links.go's verification map already needs, and a test that resolves
// each kind end to end (internal/service/request_links_test.go hands this answer
// to LinkRequest, which re-verifies it) fails if the two ever disagree.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// RequestLinkTarget is a link target resolved from its own record: the project it
// lives in and that project's tenant.
//
// ProjectID for a project target is the project itself, so a caller checks access
// the same way for every kind. OrgID is 0 for the one case where there is no
// tenant to compare — a project that predates orgs (004) has a NULL org_id — and
// a caller must treat that as "no tenant", never as a match.
type RequestLinkTarget struct {
	Kind      domain.RequestTargetKind
	ID        int
	ProjectID int
	OrgID     int
}

// ResolveRequestLinkTarget reads target kind/id from its own table and returns the
// project it lives in and that project's org.
//
// It decides nothing: a target in another org is returned with that org, not
// refused, because "is this the request's tenant?" is the caller's question and its
// answer (the request surface's single 404) belongs to the service. What it does
// report is a target that does not exist at all, as ErrNotFound, so a caller can
// tell "no such row" from a failure instead of matching on a message.
//
// A request target resolves with ProjectID 0: a request is not in a project (that
// is the whole point of routing), so there is no project to check access against.
func (r *Repo) ResolveRequestLinkTarget(ctx context.Context, kind domain.RequestTargetKind, id int) (*RequestLinkTarget, error) {
	if !isRequestTargetKind(kind) {
		return nil, fmt.Errorf("target kind %q is not one of %v: a link target has no table to resolve", kind, domain.RequestTargetKinds)
	}
	if id <= 0 {
		// The serials start at 1, so a non-positive id names no row: an ordinary
		// missing target rather than a caller error worth its own error.
		return nil, fmt.Errorf("%s %d: %w", kind, id, ErrNotFound)
	}
	var projectID int
	var org *int
	err := r.db.QueryRow(ctx, requestLinkTargetQueries[kind], id).Scan(&projectID, &org)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%s %d: %w", kind, id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve %s %d: %w", kind, id, err)
	}
	t := &RequestLinkTarget{Kind: kind, ID: id, ProjectID: projectID}
	if org != nil {
		t.OrgID = *org
	}
	return t, nil
}

// requestLinkTargetQueries resolves (project_id, org_id) for one target id, per
// target kind. A project is its own project, and its org is nullable (a pre-004
// project has none) so every scan target for org is a pointer.
//
// A request target answers project 0 on purpose: a request is routed to a project
// rather than living in one, so there is no project whose access could be checked.
var requestLinkTargetQueries = map[domain.RequestTargetKind]string{
	domain.RequestTargetProject:  `SELECT p.id, p.org_id FROM projects p WHERE p.id = $1`,
	domain.RequestTargetEpic:     `SELECT p.id, p.org_id FROM epics e JOIN projects p ON p.id = e.project_id WHERE e.id = $1`,
	domain.RequestTargetStory:    `SELECT p.id, p.org_id FROM stories s JOIN projects p ON p.id = s.project_id WHERE s.id = $1`,
	domain.RequestTargetTask:     `SELECT p.id, p.org_id FROM tasks t JOIN projects p ON p.id = t.project_id WHERE t.id = $1`,
	domain.RequestTargetArtifact: `SELECT p.id, p.org_id FROM artifacts a JOIN projects p ON p.id = a.project_id WHERE a.id = $1`,
	domain.RequestTargetReceipt:  `SELECT p.id, p.org_id FROM task_receipts r JOIN projects p ON p.id = r.project_id WHERE r.id = $1`,
	domain.RequestTargetRequest:  `SELECT 0::int, tr.org_id FROM team_requests tr WHERE tr.id = $1`,
}

// isRequestTargetKind reports whether kind is one of the seven the contract allows
// (domain.RequestTargetKinds), so an unknown kind is refused by name rather than
// looked up in a map with no entry.
func isRequestTargetKind(kind domain.RequestTargetKind) bool {
	for _, k := range domain.RequestTargetKinds {
		if k == kind {
			return true
		}
	}
	return false
}
