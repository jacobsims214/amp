package service

import (
	"context"
	"errors"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// Team submit-grant administration (AMP #3769; approved contract artifact 36
// v2 §2 and decision D14, build slice B2's service half).
//
// A submit grant lets one person send requests to one team's inbox without
// being a member of that team — and therefore without reaching any of its
// projects. The row, its tenancy constraints and its atomic revocation are
// #3768's (internal/repository/request_grants.go; migration 059); this file is
// the only place that decides **who may administer** a grant, and records that
// they did.
//
// Four properties this service is responsible for:
//
//  1. Only a team manager administers submit access (D14): a lead of the team,
//     an owner/admin of its org, or a global admin — exactly
//     authz.Checker.RequireTeamManager, the same gate AddTeamMember and
//     RemoveTeamMember use (orgs.go). A plain team member, a person outside
//     the org, a lead of some other team, a customer account and an agent
//     account with no role of its own are all refused, whatever channel they
//     come through: AMP has no "on behalf of" fallback that lends an agent
//     its manager's roles (an agent holds its own access, and its audit rows
//     name the person it acted for). The gate runs before the recipient is
//     resolved, so a caller who may not administer the team never learns
//     whether an address has an AMP account.
//  2. Every grant and revocation is audited with the acting principal, its
//     channel and the team as the target, and with the grant's recipient id —
//     never a credential. The rest of the row (actor type, acting agent and
//     the person it acts for, auth method, token id, client, source IP,
//     request id) comes from the principal and the request context, exactly as
//     it does for every other audited action (audit.go, audit_context.go).
//  3. Nothing here changes project access. Granting creates no org or team
//     membership row — the repository touches neither table — and this service
//     calls nothing that would, so a customer with a grant still reaches zero
//     projects. That separation is the reason the grant exists instead of a
//     team role (migration 059's comment, domain.TeamSubmitGrant).
//  4. A grant confers no project authority, so nothing here can invalidate a
//     remote session's access; unlike RemoveTeamMember it does not call
//     endRemoteAfterAccessChange, because there is nothing to end.
//
// Refusals are not audited here: they are returned, and the listener that
// served them records them once as access.denied (api/denied.go,
// mcp/denied.go), like every other service refusal. That keeps one row per
// refused call instead of two.
//
// There is deliberately no REST, MCP or UI adapter in this file. Each is a
// later, thin slice ("later thin adapters must reuse this service"): every one
// of them must call these methods rather than re-implement the rules.

// ---- Grant / revoke / list ----

// GrantSubmitAccess gives userID submit access to teamID, attributed to the
// caller. user is "me", a numeric user id, or an email (authz.ResolveUser),
// so an adapter can pass whatever its surface accepted.
//
// Only a team manager may call it (RequireTeamManager). Granting is idempotent:
// a grant already in force keeps its original grantor and time, and a revoked
// grant is revived with this caller and time — the repository decides that, and
// the audit row reports which of the two happened (details.already_active,
// before/after), so a replay is visible as the no-op it is rather than as a new
// grant.
func (s *Service) GrantSubmitAccess(ctx context.Context, p authz.Principal, teamID int, user string) (*domain.TeamSubmitGrant, error) {
	team, err := s.loadSubmitGrantTeam(ctx, p, teamID)
	if err != nil {
		return nil, err
	}
	userID, err := s.Authz.ResolveUser(ctx, p, user)
	if err != nil {
		return nil, err
	}
	// Read the previous row first so the audit entry can say what changed (a
	// fresh grant, a replay, or a revival of a revoked one). It is a read, not
	// a check: the repository's upsert is what makes the write correct under
	// concurrency, and a grant that raced in between is still the same access.
	previous, err := s.Repo.GetTeamSubmitGrant(ctx, teamID, userID)
	if errors.Is(err, repository.ErrNotFound) {
		previous, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	alreadyActive := previous != nil && previous.Active()

	g, err := s.Repo.GrantTeamSubmit(ctx, teamID, team.OrgID, userID, p.UserRef())
	if err != nil {
		return nil, err
	}
	summary := "gave " + s.userLabel(ctx, userID) + " submit access to team " + team.Name
	if alreadyActive {
		summary += " (already in force)"
	}
	before := submitGrantValue(previous)
	s.audit(ctx, p, domain.AuditEntry{
		Action: "team.submit_grant.set", OrgID: intRef(team.OrgID), TargetType: "team", TargetID: idStr(teamID),
		TargetName: team.Name, Summary: summary,
		Details: map[string]any{"user_id": userID, "already_active": alreadyActive},
		Before:  before, After: submitGrantValue(g),
	})
	return g, nil
}

// RevokeSubmitAccess takes userID's submit access to teamID away, attributed to
// the caller. The grant row survives (revoked, not deleted), so a later grant
// revives it and the pair is never absent-then-present; every read filters
// revoked rows out, so the revocation is effective on the very next request
// with no cache to invalidate.
//
// A pair with no grant row at all is not found. Revoking a grant that is
// already revoked is the no-op it is: the first revoker and time are kept and
// the audit row says so (details.already_revoked).
func (s *Service) RevokeSubmitAccess(ctx context.Context, p authz.Principal, teamID int, user string) (*domain.TeamSubmitGrant, error) {
	team, err := s.loadSubmitGrantTeam(ctx, p, teamID)
	if err != nil {
		return nil, err
	}
	userID, err := s.Authz.ResolveUser(ctx, p, user)
	if err != nil {
		return nil, err
	}
	previous, err := s.Repo.GetTeamSubmitGrant(ctx, teamID, userID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, authz.NotFound("team %d has no submit grant for %s", teamID, s.userLabel(ctx, userID))
	}
	if err != nil {
		return nil, err
	}
	alreadyRevoked := !previous.Active()
	g, err := s.Repo.RevokeTeamSubmit(ctx, teamID, userID, p.UserRef())
	if err != nil {
		return nil, err
	}
	summary := "took away submit access to team " + team.Name + " from " + s.userLabel(ctx, userID)
	if alreadyRevoked {
		summary += " (already revoked)"
	}
	s.audit(ctx, p, domain.AuditEntry{
		Action: "team.submit_grant.revoke", OrgID: intRef(team.OrgID), TargetType: "team", TargetID: idStr(teamID),
		TargetName: team.Name, Summary: summary,
		Details: map[string]any{"user_id": userID, "already_revoked": alreadyRevoked},
		Before:  submitGrantValue(previous), After: submitGrantValue(g),
	})
	return g, nil
}

// ListSubmitGrants returns a team's submit grants — in force and revoked, in
// force first — with each recipient's email, display name and account kind, for
// the administration view ("who may submit to this team"). Revoked grants are
// included on purpose: to a manager, "who may submit" and "who was taken away"
// are the same listing, and the row itself says which is which.
//
// Managers only, like grant and revoke: the listing names everyone with submit
// access to the team, which is administration (contract §2), not something a
// plain team member's inbox access includes.
func (s *Service) ListSubmitGrants(ctx context.Context, p authz.Principal, teamID int) ([]domain.TeamSubmitGrantee, error) {
	if _, err := s.loadSubmitGrantTeam(ctx, p, teamID); err != nil {
		return nil, err
	}
	return s.Repo.ListTeamSubmitGrants(ctx, teamID)
}

// ---- Helpers ----

// loadSubmitGrantTeam loads a team whose submit grants p may administer: it must
// exist, and p must be one of its managers. A team in another org, or one that
// doesn't exist, is reported as not found by loadTeam, so a cross-org caller
// learns nothing about another tenant's team (the same rule RequireProject
// follows for projects).
func (s *Service) loadSubmitGrantTeam(ctx context.Context, p authz.Principal, teamID int) (*domain.Team, error) {
	team, _, err := s.loadTeam(ctx, p, teamID)
	if err != nil {
		return nil, err
	}
	if err := s.Authz.RequireTeamManager(ctx, p, team); err != nil {
		return nil, err
	}
	return team, nil
}

// submitGrantValue is a submit grant's state as an audit before/after value:
// no row at all, revoked, or in force. Kept to those three words so the audit
// UI's diff reads plainly and so no column of the row (or anything that looks
// like a credential) reaches the log.
func submitGrantValue(g *domain.TeamSubmitGrant) map[string]any {
	switch {
	case g == nil:
		return map[string]any{"submit_grant": nil}
	case g.Active():
		return map[string]any{"submit_grant": "in force"}
	default:
		return map[string]any{"submit_grant": "revoked"}
	}
}
