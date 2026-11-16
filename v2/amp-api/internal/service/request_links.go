package service

// Team requests: the one authorized path that attaches a request to the work
// that serves it (epic 202, story 549, AMP #3962; approved product contract
// artifact 36 v2 §3 and §2; build slice E1 of
// discoveries/team-inbox-approved-build-slices.md; the relation contract in
// discoveries/team-request-planning-lineage.md #3656).
//
// A link is the durable statement "this request is routed to that project / is
// planned as that epic, story or ticket / was delivered by that ticket, artifact
// or receipt". It lives in `request_links` (migration 055) and the repository is
// its only write path (#3875): it rides 055's unique key so a replay is a no-op,
// verifies that the target's own table puts it in the org the caller resolved,
// refuses a second active route (D9) unless the caller supersedes it, stamps
// `planned_at` on a first `planned_as`, and appends exactly one `link` history
// row (I6).
//
// What this file adds is the part the repository deliberately does not do — it
// says so in its own header — the authorization question: *may this caller attach
// this target to this request?* Four checks, each from rows and never from
// anything the caller says about itself or its target:
//
//  1. The inbox. The caller acts in one team (a team id only — the org is read
//     from the team's own row, so a caller cannot aim a write at another tenant
//     by pairing two ids that disagree, the rule SubmitRequest follows). The
//     request must be in that team's inbox; a request in another team, a request
//     the caller has no standing on and one that does not exist are the surface's
//     single 404.
//  2. The capability. Attaching is a TRIAGE act, so it is the team/manager branch
//     of RequestCapabilityFor — the same per-call resolution the lifecycle uses
//     (authz.InboxRole + RequireTeamManager, re-read on every call, nothing
//     cached). A requester sees their own request and may not attach work to it.
//  3. The target's REAL tenant. The repository requires the org the caller
//     resolved from the target's own record, so this file resolves it that way
//     (repository.ResolveRequestLinkTarget) and hands that value on, which the
//     repository then independently re-verifies against the same table. Metadata
//     is never consulted: effective metadata is inherited project → epic → story
//     → task and a JSONB patch can rewrite it, so a key saying `org_id` is not a
//     tenant check (055's header; domain/metadata.go's merge_for rule). A target
//     in another org is answered exactly like a target that does not exist.
//  4. The target's access. Being in the same org is not enough: the caller must
//     be able to SEE the work it is attaching. RequireProject(Member) answers
//     NOT FOUND for a project the caller cannot see, so a hidden project and a
//     missing one are the same answer here, as they are everywhere else in AMP.
//
// Nothing here creates work, approves it or starts it. This is the attach half of
// the planning handoff: it links a request to work that already exists (the
// handoff that CREATES tickets, with its retry ledger, is #3965's). A successful
// call has exactly three effects — one request_links row, one request_events row
// and one audit row — and a linked ticket that awaited approval still awaits it
// afterwards (TestAttachRequestNeverApprovesOrDispatches drives the real dispatch
// gate rather than trusting this file's intentions).
//
// What this file deliberately does not do — each one another slice's concern:
//
//   - no endpoint, no MCP tool, no UI (F5/I3/J2 adapt this method).
//   - no detach. An `unlink` is the lifecycle's: reopening a duplicate detaches
//     its relation in the same statement as its state (DetachDuplicate), and a
//     planning detachment is a deliberate act with its own surface and its own
//     rev check. Adding a second verb here would put two policies in one file.
//   - no duplicate_of, and no request target. A fold moves the request's
//     duplicate_of column and its state in one statement (053's CHECK), so the
//     lifecycle owns it (MarkDuplicate); and relating two requests
//     (`relates_to` → request) is a cross-request act whose visibility rule no
//     approved text pins for v1, so it is refused by name rather than granted by
//     analogy. This slice's targets are the ticket's: project, epic, story,
//     task, artifact and receipt.
//   - no state gate. Whether a request in a closed state may receive a link is a
//     lifecycle rule and the one transition policy (#3953/#3960) owns it;
//     inventing a state check here would be a rule nobody approved (the same
//     reasoning request_uploads.go gives for uploads).
//   - no delivery. A `delivered_by` link is evidence a delivery may later count
//     (I2, #3960's RequestDeliveryEvidence); writing one does not deliver the
//     request, move its state or bump its rev.
//   - no rev check. LinkRequest deliberately does not compare-and-set the
//     request's rev: a link is lineage, not a versioned edit of the request's
//     brief, so attaching must not invalidate a client's copy of it.
//   - no metadata. This path writes no `request_ref` key. A typed row cannot be
//     inherited or clobbered, which is the whole reason 055 exists; a metadata
//     label is a reporting convenience for another slice to add.

import (
	"context"
	"errors"
	"fmt"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// AttachRequestInput is one link the caller wants between a request and a piece
// of work, as a surface receives it.
//
// There is deliberately no org, no actor, no resolved target org and no "target
// exists" claim: the caller names what it means (a request in an inbox, a relation,
// a target id) and every fact about the two rows is read here. A surface that
// passed an org would be handing this service the very value the tenant check
// exists to verify.
type AttachRequestInput struct {
	// RequestID is the request, and TeamID the inbox the caller is attaching in.
	// Both are required; the org is the team's, read from its row.
	RequestID int
	TeamID    int

	// LinkKind and TargetKind are the relation and what it points at. The pair is
	// checked against the contract's matrix (domain.NormalizeRequestLink) before
	// anything is loaded, so a `planned_as` link to an artifact is refused as the
	// invalid request it is, not stored.
	LinkKind   domain.RequestLinkKind
	TargetKind domain.RequestTargetKind
	TargetID   int

	// SupersedeRoute allows a routes_to attach that moves the request to a
	// different project than its active route (D9). Without it such an attach is
	// a 409 naming the current project — a request has one project, and changing
	// it is a deliberate act. Superseding appends a row and deletes nothing: the
	// previous routing stays in the request's lineage.
	SupersedeRoute bool
}

// AttachRequestResult is one attach.
type AttachRequestResult struct {
	// Link is the stored relation. On a replay it is the original row, with its
	// first attribution (created_by, created_at) intact.
	Link *domain.RequestLink
	// Changed is false when the relation was already there: nothing at all was
	// written — no second row, no second history row, no planned_at re-stamp, no
	// audit row — which is what makes a retried attach safe.
	Changed bool
	// Target is the target row this call resolved: the project it lives in and its
	// real org. A surface can report what the request now points at without a
	// second read, and nothing in it is derived from the caller's words. It is nil
	// on a replay of a relation whose target has since been deleted — there is
	// nothing left to resolve, and the relation that answers the replay is the
	// stored row.
	Target *repository.RequestLinkTarget
}

// AttachRequest attaches one relation between a request and the work that serves
// it, after authorizing the caller for both ends.
//
// A refusal costs nothing and writes nothing: the pair is validated before
// anything is loaded, the caller's standing comes from the rows, and the target's
// tenant and visibility are resolved before the write begins. A caller with no
// standing on the request learns nothing — a request in another team, a request
// that does not exist and a request the caller may not see are the same 404 — and
// a target in another org is answered as a target that is not there.
func (s *Service) AttachRequest(ctx context.Context, p authz.Principal, in AttachRequestInput) (*AttachRequestResult, error) {
	// A submit-scoped credential may reach exactly one operation (its bound team's
	// submission, authz/token_scope.go); an attach on an existing request is not
	// it. Refusing here names the credential's limit instead of hiding it behind a
	// 404, and it is the answer the surface gate gives too.
	if err := p.RequireUnscoped("attach work to a request"); err != nil {
		return nil, err
	}

	// The relation is validated first because it is free to check and the domain
	// owns the matrix: a pair the contract never allows cannot be stored, and
	// answering that as "not found" would blame the target for a caller's mistake.
	kind, target, err := domain.NormalizeRequestLink(string(in.LinkKind), string(in.TargetKind))
	if err != nil {
		return nil, authz.RequestInvalid("%s", err)
	}
	switch {
	case kind == domain.RequestLinkDuplicateOf:
		// A fold moves the request's duplicate_of column and its state in one
		// statement (053's CHECK); writing the relation alone would claim a fold
		// the record does not have. That act is the lifecycle's.
		return nil, authz.RequestInvalid(
			"a duplicate_of relation is not attached: marking request %d a duplicate of another moves its state and its canonical pointer together, which the duplicate transition owns (D7)", in.RequestID)
	case target == domain.RequestTargetRequest:
		// Relating two requests is a link between two request records, not a link
		// to work, and its visibility rule (may a triager of one inbox relate a
		// request of another?) is not pinned by the approved v1 text. Refused by
		// name rather than granted by analogy: this path attaches work, and a
		// relation between requests gets the decision it needs, not a default.
		return nil, authz.RequestInvalid(
			"attaching request %d to another request is not a work attach: relates_to between requests is its own act with its own visibility rule, and this path attaches projects, epics, stories, tickets, artifacts and receipts", in.RequestID)
	}
	if in.TargetID <= 0 {
		return nil, authz.RequestInvalid("%s target_id must name the %s this request is linked to (got %d)", target, target, in.TargetID)
	}

	team, req, capability, err := s.loadInboxRequest(ctx, p, in.RequestID, in.TeamID)
	if err != nil {
		return nil, err
	}
	// Attaching is triage: the inbox team and its managers. A requester is refused
	// by name rather than by 404 because they may already see the request they are
	// asking about (RequestViewFor gives them their own), so naming the rule tells
	// them something true without telling them anything new.
	if !capability.canMake(RequestCapabilityTeam) {
		if capability == RequestCapabilityNone {
			return nil, authz.RequestNotFound()
		}
		return nil, authz.Forbidden(
			"attaching work to request %s is the inbox team's act: being its requester means following it and answering it, not planning what serves it. Nothing was linked.", req.PublicRef)
	}

	// The request's own lineage is read once, for one purpose: to answer a replay
	// whose target no longer exists (see the ErrNotFound branch below). It is not
	// an authorization read — the capability check above already decided the caller
	// — and it is not the writes' existence check either: the repository decides
	// replay-versus-new inside the transaction that writes, so a lost race still
	// converges. This read only lets "the target is gone" be told apart from "the
	// relation never existed", which is the difference between a harmless retry
	// and a 404.
	links, err := s.Repo.RequestLinks(ctx, req.ID, req.OrgID, req.TeamID, kind)
	if err != nil {
		return nil, fmt.Errorf("read request %d's %s links: %w", req.ID, kind, err)
	}

	// The target is read from its own record, which is the only source of its
	// tenant: the repository re-verifies this exact org against the same table
	// before the insert, so a value read out of metadata (or guessed) cannot pass.
	resolved, err := s.Repo.ResolveRequestLinkTarget(ctx, target, in.TargetID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// A target that is not there is normally a 404 — but a relation that is
			// ALREADY stored is already stored, even if the thing it names has since
			// been deleted. That is what a tombstone is (055: "we delivered this
			// request with ticket #3218, then the plan changed and the ticket was
			// deleted"), and re-resolving a gone target would turn a harmless retry
			// into a 404. The repository makes exactly this ordering choice inside
			// its own write; this service has to make it here too, because it
			// resolves the target before calling the write.
			if existing, ok := existingLink(links, kind, target, in.TargetID); ok {
				return &AttachRequestResult{Link: existing, Changed: false}, nil
			}
			return nil, authz.NotFound("%s %d not found", target, in.TargetID)
		}
		return nil, fmt.Errorf("resolve %s %d for request %d: %w", target, in.TargetID, req.ID, err)
	}
	if resolved.OrgID == 0 || resolved.OrgID != req.OrgID {
		// Another tenant's work — or work in no tenant at all — answered exactly as
		// work that is not there. The request's org is the team's org (053's
		// composite key makes the pair one fact), so this compares two typed
		// columns and nothing else.
		return nil, authz.NotFound("%s %d not found", target, in.TargetID)
	}
	if _, err := s.Authz.RequireProject(ctx, p, resolved.ProjectID, authz.Member); err != nil {
		if authz.StatusOf(err, 0) == 404 {
			// A project this caller may not see: answer for the target it named, so
			// the refusal names no project id the caller had no right to learn.
			return nil, authz.NotFound("%s %d not found", target, in.TargetID)
		}
		return nil, fmt.Errorf("check access to project %d for %s %d: %w", resolved.ProjectID, target, in.TargetID, err)
	}

	link, changed, err := s.Repo.LinkRequest(ctx, repository.RequestLinkWrite{
		RequestID:  req.ID,
		OrgID:      req.OrgID,
		TeamID:     req.TeamID,
		LinkKind:   kind,
		TargetKind: target,
		TargetID:   in.TargetID,
		// The org this file resolved from the target's own row and has already
		// compared with the request's.
		TargetOrgID:    resolved.OrgID,
		SupersedeRoute: in.SupersedeRoute,
		CreatedBy:      p.UserRef(),
		Actor:          p.Label(),
		ActorUserID:    p.UserRef(),
	})
	if err != nil {
		return nil, attachRequestError(req, kind, target, in.TargetID, err)
	}
	if changed {
		s.recordRequestLink(ctx, p, team, req, link, resolved)
	}
	return &AttachRequestResult{Link: link, Changed: changed, Target: resolved}, nil
}

// existingLink finds the stored relation a replay would name, among the request's
// links of that kind. It is deliberately a plain scan rather than a query: the
// caller has already read the request's lineage for the kind, and the ties 055's
// unique key allows (one row per request, target and kind) make this exact.
func existingLink(links []domain.RequestLink, kind domain.RequestLinkKind,
	target domain.RequestTargetKind, targetID int) (*domain.RequestLink, bool) {
	for i := range links {
		if links[i].LinkKind == kind && links[i].TargetKind == target && links[i].TargetID == targetID {
			return &links[i], true
		}
	}
	return nil, false
}

// loadInboxRequest loads the inbox the caller is acting in, the request inside
// it, and the caller's capability on that request — the same three answers, in
// the same order, that every request write starts from, so an attach and a
// transition cannot disagree about who may act where.
//
// It is a named alias rather than a second implementation: the loader lives in
// request_lifecycle.go (it was written for the lifecycle write, #3960), and a
// copy here would be two places to fix the day the capability resolution changes.
func (s *Service) loadInboxRequest(ctx context.Context, p authz.Principal, requestID, teamID int) (*domain.Team, *domain.TeamRequest, RequestCapability, error) {
	return s.loadTransitionTarget(ctx, p, TransitionRequestInput{RequestID: requestID, TeamID: teamID})
}

// attachRequestError translates the repository's answers into the request
// surface's stable codes, and leaves every other failure as itself so a dead
// connection or a cancelled context still reads as what it is.
//
//   - A second active route is a 409 (request.conflict) that names the project
//     the request is on now: D9 is a rule the caller can resolve by deciding to
//     supersede, and naming the current project is the fact that decision needs.
//   - A target that vanished between this file's resolve and the write is the
//     surface's one 404: the work really is not there any more.
//   - ErrRequestLinkTargetIdentity is NOT mapped: it says this service handed the
//     repository an org the target's table disagrees with, which is a bug here
//     (this file resolves that org from the same table) and must reach the log as
//     a fault rather than be dressed up as a refusal.
func attachRequestError(req *domain.TeamRequest, kind domain.RequestLinkKind, target domain.RequestTargetKind, targetID int, err error) error {
	var route *repository.RequestRouteConflict
	switch {
	case errors.As(err, &route):
		return &codedCause{
			err: authz.RequestConflict(
				"request %s is routed to project %d: a request has one project (D9). To move it to project %d deliberately, supersede the current route — that appends the new routing and keeps the old one as history",
				req.PublicRef, route.CurrentID, route.WantedID),
			cause: repository.ErrConflict,
		}
	case errors.Is(err, repository.ErrNotFound):
		return authz.NotFound("%s %d not found", target, targetID)
	case errors.Is(err, repository.ErrRequestLinkDuplicate):
		return authz.RequestInvalid("%s", err)
	}
	return fmt.Errorf("attach %s %d to request %d: %w", kind, targetID, req.ID, err)
}

// recordRequestLink writes the one audit row an attach gets, after it committed:
// `request.link`, on the request's org, targeting the request.
//
// It runs after the commit and how it fails is audit.go's rule: the relation is
// already stored, so a failure is logged rather than returned. A replay never
// reaches here — changed=false means nothing was written, and an audit row for a
// no-op would say an act happened that did not.
//
// The details are ids and kinds only. A link's target may be a ticket whose name
// or a project whose title is internal, and a request's history is requester-
// visible (055's rule for its own event details), so nothing here names one.
func (s *Service) recordRequestLink(ctx context.Context, p authz.Principal, team *domain.Team,
	req *domain.TeamRequest, link *domain.RequestLink, target *repository.RequestLinkTarget) {
	s.audit(ctx, p, domain.AuditEntry{
		Action:     "request.link",
		OrgID:      intRef(team.OrgID),
		TargetType: "request",
		TargetID:   idStr(req.ID),
		TargetName: req.PublicRef,
		Summary:    "linked request " + req.PublicRef + " to " + string(link.TargetKind) + " " + idStr(link.TargetID),
		Details: map[string]any{
			"team_id":     team.ID,
			"public_ref":  req.PublicRef,
			"link_id":     link.ID,
			"link_kind":   string(link.LinkKind),
			"target_kind": string(link.TargetKind),
			"target_id":   link.TargetID,
			"project_id":  target.ProjectID,
		},
	})
}
