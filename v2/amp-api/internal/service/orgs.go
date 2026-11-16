package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/repository"
)

// Org and team administration, shared by the REST API (/api/orgs, /api/teams)
// and the MCP admin tools, so an admin's agent is held to exactly the rules
// the UI is:
//
//   - only global admins create or delete orgs;
//   - org owners/admins manage members, teams, invites; only org owners
//     grant, change, or remove the owner role;
//   - team leads manage their own team's members (and may invite into it);
//   - linking teams to a project needs project owner access.

var (
	validOrgRoles  = map[string]bool{domain.OrgRoleOwner: true, domain.OrgRoleAdmin: true, domain.OrgRoleMember: true}
	validTeamRoles = map[string]bool{domain.TeamRoleLead: true, domain.TeamRoleMember: true}
	slugRe         = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// MemberResult says what adding someone did: added them now, or (for an
// email that has never signed in) recorded an invite.
type MemberResult struct {
	Added   bool           `json:"added"`
	UserID  int            `json:"user_id,omitempty"`
	Role    string         `json:"role,omitempty"`
	Invited bool           `json:"invited"`
	Invite  *domain.Invite `json:"invite,omitempty"`
}

// orgAccess is the caller's standing in an org.
type orgAccess struct {
	role  string
	admin bool // org owner/admin or global admin
	owner bool // org owner or global admin
}

func (s *Service) orgAccess(ctx context.Context, p authz.Principal, orgID int) (orgAccess, error) {
	role, err := s.Authz.RequireOrgMember(ctx, p, orgID)
	if err != nil {
		return orgAccess{}, err
	}
	return orgAccess{
		role:  role,
		admin: p.Super || role == domain.OrgRoleOwner || role == domain.OrgRoleAdmin,
		owner: p.Super || role == domain.OrgRoleOwner,
	}, nil
}

func (a orgAccess) requireAdmin(orgID int) error {
	if !a.admin {
		return authz.Forbidden("org %d: requires org owner or admin", orgID)
	}
	return nil
}

// ---- Orgs ----

// ListOrgs returns the caller's orgs (global admins: all), with their role in each.
func (s *Service) ListOrgs(ctx context.Context, p authz.Principal) ([]domain.Org, error) {
	if !p.Super {
		return s.Repo.ListOrgs(ctx, p.UserRef())
	}
	orgs, err := s.Repo.ListOrgs(ctx, nil)
	if err != nil || !p.HasUser() {
		return orgs, err
	}
	mine, err := s.Repo.ListOrgs(ctx, p.UserRef())
	if err != nil {
		return nil, err
	}
	roles := map[int]string{}
	for _, o := range mine {
		roles[o.ID] = o.MyRole
	}
	for i := range orgs {
		orgs[i].MyRole = roles[orgs[i].ID]
	}
	return orgs, nil
}

// GetOrg returns an org the caller belongs to, with their role.
func (s *Service) GetOrg(ctx context.Context, p authz.Principal, orgID int) (*domain.Org, error) {
	a, err := s.orgAccess(ctx, p, orgID)
	if err != nil {
		return nil, err
	}
	org, err := s.Repo.GetOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	org.MyRole = a.role
	return org, nil
}

// CreateOrg creates an org (global admins only). owner is a user id or
// email; it defaults to the caller. An email that has never signed in gets
// an owner invite instead.
func (s *Service) CreateOrg(ctx context.Context, p authz.Principal, name, slug, owner string) (*domain.Org, *domain.Invite, error) {
	if !p.Super {
		return nil, nil, authz.Forbidden("only global admins can create orgs")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil, authz.BadRequest("name is required")
	}
	if slug == "" {
		slug = Slugify(name)
	}
	if !slugRe.MatchString(slug) {
		return nil, nil, authz.BadRequest("slug must be lowercase letters, digits and dashes")
	}
	ownerID := p.UserRef()
	var inviteOwner string
	if owner != "" {
		id, err := s.Authz.ResolveUser(ctx, p, owner)
		switch {
		case err == nil:
			// The owner role is a grant path like any other: a customer
			// account can't be made the org's first owner (#3766), which
			// the repository also refuses inside its writing transaction.
			if err := s.requireKindCanHoldOrgRole(ctx, id, domain.OrgRoleOwner); err != nil {
				return nil, nil, err
			}
			ownerID = &id
		case strings.Contains(owner, "@"):
			// No AMP user yet: the invite will grant the role when it is
			// accepted, and AcceptInvitesList re-checks the kind then.
			inviteOwner = owner
		default:
			return nil, nil, err
		}
	}
	org, err := s.Repo.CreateOrg(ctx, name, slug, p.UserRef(), ownerID)
	if err != nil {
		return nil, nil, err
	}
	if inviteOwner == "" {
		s.audit(ctx, p, domain.AuditEntry{Action: "org.create", OrgID: intRef(org.ID), TargetType: "org", TargetID: idStr(org.ID),
			Summary: "created org " + org.Name, Details: map[string]any{"owner_id": ownerID}})
		return org, nil, nil
	}
	inv, err := s.Repo.CreateInvite(ctx, domain.Invite{
		Email: strings.ToLower(strings.TrimSpace(inviteOwner)), OrgID: org.ID,
		OrgRole: domain.OrgRoleOwner, TeamRole: domain.TeamRoleMember, InvitedBy: p.UserRef(),
	})
	if err == nil {
		s.audit(ctx, p, domain.AuditEntry{Action: "org.create", OrgID: intRef(org.ID), TargetType: "org", TargetID: idStr(org.ID),
			Summary: "created org " + org.Name + ", owner invited: " + inv.Email})
	}
	return org, inv, err
}

func (s *Service) RenameOrg(ctx context.Context, p authz.Principal, orgID int, name string) (*domain.Org, error) {
	a, err := s.orgAccess(ctx, p, orgID)
	if err != nil {
		return nil, err
	}
	if err := a.requireAdmin(orgID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, authz.BadRequest("name is required")
	}
	var before map[string]any
	if prev, err := s.Repo.GetOrg(ctx, orgID); err == nil {
		before = map[string]any{"name": prev.Name}
	}
	if err := s.Repo.RenameOrg(ctx, orgID, strings.TrimSpace(name)); err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "org.rename", OrgID: intRef(orgID), TargetType: "org", TargetID: idStr(orgID),
		TargetName: strings.TrimSpace(name), Summary: "renamed org to " + strings.TrimSpace(name),
		Before: before, After: map[string]any{"name": strings.TrimSpace(name)}})
	return s.Repo.GetOrg(ctx, orgID)
}

func (s *Service) DeleteOrg(ctx context.Context, p authz.Principal, orgID int) error {
	if _, err := s.orgAccess(ctx, p, orgID); err != nil {
		return err
	}
	if !p.Super {
		return authz.Forbidden("only global admins can delete orgs")
	}
	teams, err := s.Repo.ListTeams(ctx, orgID)
	if err != nil {
		return err
	}
	if err := s.Repo.DeleteOrg(ctx, orgID); err != nil {
		return err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "org.delete", OrgID: intRef(orgID), TargetType: "org", TargetID: idStr(orgID),
		Summary: "deleted the org and its teams", Details: map[string]any{"teams": len(teams)}})
	// The org's teams went with it; their and its knowledge bases go to
	// the trash (kept 30 days in Postgres, then purged).
	s.dropKB(kb.Org(orgID))
	for _, t := range teams {
		s.dropKB(kb.Team(t.ID))
	}
	return nil
}

// ---- Org members ----

func (s *Service) ListOrgMembers(ctx context.Context, p authz.Principal, orgID int) ([]domain.Member, error) {
	if _, err := s.orgAccess(ctx, p, orgID); err != nil {
		return nil, err
	}
	return s.Repo.ListOrgMembers(ctx, orgID)
}

// AddOrgMember adds a user to an org or changes their role. An email with
// no AMP user yet is invited (only when invite is true — i.e. adding, not
// changing a role).
func (s *Service) AddOrgMember(ctx context.Context, p authz.Principal, orgID int, user, role string, invite bool) (*MemberResult, error) {
	a, err := s.orgAccess(ctx, p, orgID)
	if err != nil {
		return nil, err
	}
	if err := a.requireAdmin(orgID); err != nil {
		return nil, err
	}
	if role == "" {
		role = domain.OrgRoleMember
	}
	if !validOrgRoles[role] {
		return nil, authz.BadRequest("role must be owner, admin, or member")
	}
	if role == domain.OrgRoleOwner && !a.owner {
		return nil, authz.Forbidden("only org owners can grant the owner role")
	}
	userID, err := s.Authz.ResolveUser(ctx, p, user)
	if err != nil {
		if invite && strings.Contains(user, "@") {
			return s.invite(ctx, p, domain.Invite{Email: user, OrgID: orgID, OrgRole: role, TeamRole: domain.TeamRoleMember})
		}
		return nil, err
	}
	// A customer may be in an org as a plain member, never as its owner or
	// admin: org owner/admin reaches every project in the org, and a
	// requester identity must reach none (#3766). The repository refuses the
	// same grant inside its writing transaction, so no other path slips past
	// this clear answer.
	if err := s.requireKindCanHoldOrgRole(ctx, userID, role); err != nil {
		return nil, err
	}
	current, err := s.Repo.GetOrgRole(ctx, orgID, userID)
	if err != nil {
		return nil, err
	}
	if current == domain.OrgRoleOwner && !a.owner {
		return nil, authz.Forbidden("only org owners can change an owner's role")
	}
	if err := s.Repo.SetOrgMember(ctx, orgID, userID, role); err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "org.member.set", OrgID: intRef(orgID), TargetType: "user", TargetID: idStr(userID),
		Summary: "set " + s.userLabel(ctx, userID) + " to org " + role + roleChange(current), Details: map[string]any{"role": role, "previous": current},
		Before: roleValue(current), After: roleValue(role)})
	s.endRemoteAfterAccessChange(ctx, p, userID) // an org admin made a plain member
	return &MemberResult{Added: true, UserID: userID, Role: role}, nil
}

func (s *Service) RemoveOrgMember(ctx context.Context, p authz.Principal, orgID int, user string) (int, error) {
	a, err := s.orgAccess(ctx, p, orgID)
	if err != nil {
		return 0, err
	}
	if err := a.requireAdmin(orgID); err != nil {
		return 0, err
	}
	userID, err := s.Authz.ResolveUser(ctx, p, user)
	if err != nil {
		return 0, err
	}
	current, err := s.Repo.GetOrgRole(ctx, orgID, userID)
	if err != nil {
		return 0, err
	}
	if current == domain.OrgRoleOwner && !a.owner {
		return 0, authz.Forbidden("only org owners can remove an owner")
	}
	if err := s.Repo.RemoveOrgMember(ctx, orgID, userID); err != nil {
		return 0, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "org.member.remove", OrgID: intRef(orgID), TargetType: "user", TargetID: idStr(userID),
		Summary: "removed " + s.userLabel(ctx, userID) + " from the org (was " + current + ")", Details: map[string]any{"previous": current},
		Before: roleValue(current), After: roleValue("")})
	s.endRemoteAfterAccessChange(ctx, p, userID)
	return userID, nil
}

// requireKindCanHoldOrgRole refuses an org owner/admin role for a customer
// account, with the same clear message as the repository's writing-transaction
// guard (#3766). Any other account, and any other org role, is unaffected.
func (s *Service) requireKindCanHoldOrgRole(ctx context.Context, userID int, role string) error {
	if role != domain.OrgRoleOwner && role != domain.OrgRoleAdmin {
		return nil
	}
	u, err := s.Repo.GetUserByID(ctx, userID)
	if err != nil {
		return authz.NotFound("user %d not found", userID)
	}
	if !domain.CanHoldOrgRole(u.Kind, role) {
		return authz.BadRequest("a customer account can't be org %s: customers submit requests, they never administer an org (%s)", role, u.Email)
	}
	return nil
}

// requireKindCanBeTeamMember refuses team membership for a customer account,
// with the same conflict (409) and the same shape as the invite-acceptance
// path's refusal (Repo.AcceptInvitesList, #3861). Team membership is how a
// person reaches every project the team is linked to, which a requester
// identity must never get; a customer reaches a team's inbox through a submit
// grant instead (Service.GrantSubmitAccess). Any other account kind is
// unaffected, and so is every human path.
//
// The repository refuses the same grant inside its writing transaction
// (guardCustomerTeamMember), so this is the clear answer, not the only guard.
func (s *Service) requireKindCanBeTeamMember(ctx context.Context, userID int) error {
	u, err := s.Repo.GetUserByID(ctx, userID)
	if err != nil {
		return authz.NotFound("user %d not found", userID)
	}
	if u.IsCustomer() {
		return &authz.Error{Status: 409, Msg: fmt.Sprintf(
			"a customer account can't join a team: team membership reaches projects, and customers submit requests (%s); give them submit access to the team's inbox instead", u.Email)}
	}
	return nil
}

// ---- Teams ----

func (s *Service) ListTeams(ctx context.Context, p authz.Principal, orgID int) ([]domain.Team, error) {
	if _, err := s.orgAccess(ctx, p, orgID); err != nil {
		return nil, err
	}
	return s.Repo.ListTeams(ctx, orgID)
}

func (s *Service) CreateTeam(ctx context.Context, p authz.Principal, orgID int, name string) (*domain.Team, error) {
	a, err := s.orgAccess(ctx, p, orgID)
	if err != nil {
		return nil, err
	}
	if err := a.requireAdmin(orgID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, authz.BadRequest("name is required")
	}
	team, err := s.Repo.CreateTeam(ctx, orgID, strings.TrimSpace(name), p.UserRef())
	if err == nil {
		s.audit(ctx, p, domain.AuditEntry{Action: "team.create", OrgID: intRef(orgID), TargetType: "team", TargetID: idStr(team.ID),
			Summary: "created team " + team.Name})
	}
	return team, err
}

// loadTeam fetches a team the caller can see (any member of its org).
func (s *Service) loadTeam(ctx context.Context, p authz.Principal, teamID int) (*domain.Team, orgAccess, error) {
	team, err := s.Repo.GetTeam(ctx, teamID)
	if err != nil {
		return nil, orgAccess{}, authz.NotFound("team %d not found", teamID)
	}
	a, err := s.orgAccess(ctx, p, team.OrgID)
	if err != nil {
		return nil, orgAccess{}, authz.NotFound("team %d not found", teamID)
	}
	return team, a, nil
}

func (s *Service) GetTeam(ctx context.Context, p authz.Principal, teamID int) (*domain.Team, []domain.Member, error) {
	team, _, err := s.loadTeam(ctx, p, teamID)
	if err != nil {
		return nil, nil, err
	}
	members, err := s.Repo.ListTeamMembers(ctx, teamID)
	return team, members, err
}

func (s *Service) RenameTeam(ctx context.Context, p authz.Principal, teamID int, name string) (*domain.Team, error) {
	team, a, err := s.loadTeam(ctx, p, teamID)
	if err != nil {
		return nil, err
	}
	if err := a.requireAdmin(team.OrgID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, authz.BadRequest("name is required")
	}
	if err := s.Repo.RenameTeam(ctx, teamID, strings.TrimSpace(name)); err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "team.rename", OrgID: intRef(team.OrgID), TargetType: "team", TargetID: idStr(teamID),
		Summary: "renamed team " + team.Name + " to " + strings.TrimSpace(name)})
	return s.Repo.GetTeam(ctx, teamID)
}

func (s *Service) DeleteTeam(ctx context.Context, p authz.Principal, teamID int) error {
	team, a, err := s.loadTeam(ctx, p, teamID)
	if err != nil {
		return err
	}
	if err := a.requireAdmin(team.OrgID); err != nil {
		return err
	}
	if err := s.Repo.DeleteTeam(ctx, teamID); err != nil {
		return err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "team.delete", OrgID: intRef(team.OrgID), TargetType: "team", TargetID: idStr(teamID),
		Summary: "deleted team " + team.Name})
	s.endRemoteAfterAccessChange(ctx, p) // anyone who reached a project only through it
	s.dropKB(kb.Team(teamID))
	return nil
}

// ---- Team members ----

// AddTeamMember adds a user to a team (and its org), or changes their team
// role. Team leads and org admins may do this; an unknown email is invited
// into the org and team (only when invite is true).
func (s *Service) AddTeamMember(ctx context.Context, p authz.Principal, teamID int, user, role string, invite bool) (*MemberResult, error) {
	team, _, err := s.loadTeam(ctx, p, teamID)
	if err != nil {
		return nil, err
	}
	if err := s.Authz.RequireTeamManager(ctx, p, team); err != nil {
		return nil, err
	}
	if role == "" {
		role = domain.TeamRoleMember
	}
	if !validTeamRoles[role] {
		return nil, authz.BadRequest("role must be lead or member")
	}
	userID, err := s.Authz.ResolveUser(ctx, p, user)
	if err != nil {
		if invite && strings.Contains(user, "@") {
			return s.invite(ctx, p, domain.Invite{Email: user, OrgID: team.OrgID, OrgRole: domain.OrgRoleMember, TeamID: &team.ID, TeamRole: role})
		}
		return nil, err
	}
	// A customer is not a team member by implication (#3861): team membership
	// reaches every project the team is linked to, which the approved contract
	// forbids for a requester identity (see architecture/customer-accounts.md).
	// The invite-acceptance path already refused this (AcceptInvitesList); the
	// direct add path did not until now. The repository refuses the same grant
	// inside its writing transaction, so no other caller slips past this clear
	// answer.
	if err := s.requireKindCanBeTeamMember(ctx, userID); err != nil {
		return nil, err
	}
	previous, _ := s.Repo.GetTeamRole(ctx, teamID, userID)
	if err := s.Repo.SetTeamMember(ctx, teamID, userID, role); err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "team.member.set", OrgID: intRef(team.OrgID), TargetType: "team", TargetID: idStr(teamID),
		TargetName: team.Name, Summary: "set " + s.userLabel(ctx, userID) + " to " + role + " of team " + team.Name,
		Details: map[string]any{"user_id": userID, "role": role, "previous": previous}, Before: roleValue(previous), After: roleValue(role)})
	return &MemberResult{Added: true, UserID: userID, Role: role}, nil
}

func (s *Service) RemoveTeamMember(ctx context.Context, p authz.Principal, teamID int, user string) (int, error) {
	team, _, err := s.loadTeam(ctx, p, teamID)
	if err != nil {
		return 0, err
	}
	if err := s.Authz.RequireTeamManager(ctx, p, team); err != nil {
		return 0, err
	}
	userID, err := s.Authz.ResolveUser(ctx, p, user)
	if err != nil {
		return 0, err
	}
	previous, _ := s.Repo.GetTeamRole(ctx, teamID, userID)
	if err := s.Repo.RemoveTeamMember(ctx, teamID, userID); err != nil {
		return 0, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "team.member.remove", OrgID: intRef(team.OrgID), TargetType: "team", TargetID: idStr(teamID),
		TargetName: team.Name, Summary: "removed " + s.userLabel(ctx, userID) + " from team " + team.Name,
		Details: map[string]any{"user_id": userID, "previous": previous}, Before: roleValue(previous), After: roleValue("")})
	s.endRemoteAfterAccessChange(ctx, p, userID)
	return userID, nil
}

// ---- Invites ----

func (s *Service) ListInvites(ctx context.Context, p authz.Principal, orgID int) ([]domain.Invite, error) {
	a, err := s.orgAccess(ctx, p, orgID)
	if err != nil {
		return nil, err
	}
	if err := a.requireAdmin(orgID); err != nil {
		return nil, err
	}
	return s.Repo.ListPendingInvites(ctx, orgID)
}

// Invite records a pending invitation. Org admins can invite with any role
// (owner only by owners); team leads may invite into their own team as
// plain org members.
//
// A requester invitation (kind "requester", #3767) is the customer path from
// the approved contract §5: it must name a team (the team the person will be
// able to submit to), grants no org or team role, and needs only that team's
// manager — a team lead or an org owner/admin (RequireTeamManager). An existing
// account is never turned into a customer by an invitation, so a requester
// invite naming an email that already has one is refused rather than applied.
func (s *Service) Invite(ctx context.Context, p authz.Principal, inv domain.Invite) (*MemberResult, error) {
	kind, err := domain.NormalizeInviteKind(inv.Kind)
	if err != nil {
		return nil, authz.BadRequest("%s", err)
	}
	inv.Kind = kind
	a, err := s.orgAccess(ctx, p, inv.OrgID)
	if err != nil {
		return nil, err
	}
	if inv.OrgRole == "" {
		inv.OrgRole = domain.OrgRoleMember
	}
	if inv.TeamRole == "" {
		inv.TeamRole = domain.TeamRoleMember
	}
	if inv.IsRequester() {
		return s.inviteRequester(ctx, p, a, inv)
	}
	if !a.admin {
		if inv.TeamID == nil || inv.OrgRole != domain.OrgRoleMember {
			return nil, authz.Forbidden("org %d: requires org owner or admin", inv.OrgID)
		}
		team, err := s.Repo.GetTeam(ctx, *inv.TeamID)
		if err != nil || team.OrgID != inv.OrgID {
			return nil, authz.BadRequest("team is not in this org")
		}
		if err := s.Authz.RequireTeamManager(ctx, p, team); err != nil {
			return nil, err
		}
	}
	if inv.OrgRole == domain.OrgRoleOwner && !a.owner {
		return nil, authz.Forbidden("only org owners can grant the owner role")
	}
	return s.invite(ctx, p, inv)
}

// inviteRequester stores a requester invitation: the customer-targeted half of
// the approved contract §5 (#3767). The invite is bound to one org, one team
// and one recipient email, and only that team's manager can issue it.
func (s *Service) inviteRequester(ctx context.Context, p authz.Principal, a orgAccess, inv domain.Invite) (*MemberResult, error) {
	if inv.TeamID == nil {
		return nil, authz.BadRequest("a requester invitation must name the team they can send requests to")
	}
	// The invitation is not a membership: it must not smuggle a role in.
	if inv.OrgRole != domain.OrgRoleMember || inv.TeamRole != domain.TeamRoleMember {
		return nil, authz.BadRequest("a requester invitation grants no org or team role: leave org_role and team_role at member")
	}
	team, err := s.Repo.GetTeam(ctx, *inv.TeamID)
	if err != nil || team.OrgID != inv.OrgID {
		return nil, authz.BadRequest("team is not in this org")
	}
	if err := s.Authz.RequireTeamManager(ctx, p, team); err != nil {
		return nil, err
	}
	// An existing account keeps its kind: an explicit requester invitation
	// provisions a new customer identity, it never converts or demotes a
	// person, an agent or a team member.
	if u, err := s.Repo.GetUserByEmail(ctx, inv.Email); err == nil {
		return nil, authz.BadRequest("%s already has an AMP account, so a requester invitation would not create a customer identity; only a new email can be invited as a requester", u.Email)
	}
	inv.InvitedBy = p.UserRef()
	created, err := s.Repo.CreateInvite(ctx, inv)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "invite.create", OrgID: intRef(inv.OrgID), TargetType: "invite", TargetID: idStr(created.ID),
		Summary: "invited " + inv.Email + " to send requests to team " + team.Name, Details: map[string]any{"kind": inv.Kind, "team_id": *inv.TeamID}})
	return &MemberResult{Invited: true, Invite: created}, nil
}

// ListRequesterInvites returns one team's pending requester invitations, for the
// inbox's submit-access administration view (AMP #3975): who has been invited as
// a requester and has not signed in yet. It is manager-only, the same gate as
// grant and revoke — the listing names people who will be able to send to this
// inbox, which is administration (contract §2), not something a plain team
// member's inbox access includes.
//
// It is narrower than ListInvites on purpose: that one is an org admin's whole
// pending set (membership and requester invitations together), while this one
// answers the team's own question and refuses a non-manager. A team lead with no
// org role can therefore see the requester invitations of their own inbox.
func (s *Service) ListRequesterInvites(ctx context.Context, p authz.Principal, teamID int) ([]domain.Invite, error) {
	if _, err := s.loadSubmitGrantTeam(ctx, p, teamID); err != nil {
		return nil, err
	}
	return s.Repo.ListPendingRequesterInvites(ctx, teamID)
}

// RevokeRequesterInvite revokes one pending requester invitation of teamID, for
// the same administration view (AMP #3975). Manager-only, like the listing.
//
// It is not RevokeInvite: that one is org-admin-only and takes an org id, so a
// team lead could not revoke their own inbox's invitation with it, and a
// manager of one inbox could revoke another inbox's invitation by naming its
// org. Here the team is in the path and in the delete's WHERE clause, so a
// caller who may administer team A cannot revoke a row of team B — the row
// simply isn't the one named, and the answer is not found.
//
// A row that was already accepted is not revocable (the delete keeps
// accepted_at IS NULL), which is the same rule as before: revoking deletes the
// pending intent, and there is no second copy of it once the person has signed
// in.
func (s *Service) RevokeRequesterInvite(ctx context.Context, p authz.Principal, teamID, inviteID int) error {
	team, err := s.loadSubmitGrantTeam(ctx, p, teamID)
	if err != nil {
		return err
	}
	if err := s.Repo.DeleteRequesterInvite(ctx, teamID, inviteID); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return authz.NotFound("invite %d not found for team %d", inviteID, teamID)
		}
		return err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "invite.revoke", OrgID: intRef(team.OrgID), TargetType: "invite", TargetID: idStr(inviteID),
		TargetName: team.Name, Summary: "revoked a requester invitation to team " + team.Name,
		Details: map[string]any{"kind": domain.InviteKindRequester, "team_id": teamID}})
	return nil
}

// InviteRequesterForTeam issues a requester invitation to one team's inbox,
// bound to that team and its org (AMP #3975). It exists so the team-addressed
// surface needs no second way to say the same thing: Service.Invite takes a
// whole Invite, and an adapter that fills in the org, the team and the kind from
// a path would be deciding the invitation's shape in the transport. Here the
// team is loaded and gated first, and its org is read from its own row.
//
// Everything else is inviteRequester's: the manager gate (a team lead or the
// org's owner/admin), the refusal of an email that already has an account, and
// the audit row naming the team and the kind. Accepting the invitation
// provisions a customer identity and grants no membership.
func (s *Service) InviteRequesterForTeam(ctx context.Context, p authz.Principal, teamID int, email string) (*MemberResult, error) {
	team, a, err := s.loadTeam(ctx, p, teamID)
	if err != nil {
		return nil, err
	}
	return s.inviteRequester(ctx, p, a, domain.Invite{
		Email:    strings.ToLower(strings.TrimSpace(email)),
		OrgID:    team.OrgID,
		OrgRole:  domain.OrgRoleMember,
		TeamID:   &team.ID,
		TeamRole: domain.TeamRoleMember,
		Kind:     domain.InviteKindRequester,
	})
}

// invite validates and stores an invite; if the email already belongs to
// an AMP user the membership is applied immediately instead.
func (s *Service) invite(ctx context.Context, p authz.Principal, inv domain.Invite) (*MemberResult, error) {
	inv.Email = strings.ToLower(strings.TrimSpace(inv.Email))
	if !strings.Contains(inv.Email, "@") {
		return nil, authz.BadRequest("a valid email is required")
	}
	if !validOrgRoles[inv.OrgRole] || !validTeamRoles[inv.TeamRole] {
		return nil, authz.BadRequest("invalid org_role or team_role")
	}
	if inv.TeamID != nil {
		team, err := s.Repo.GetTeam(ctx, *inv.TeamID)
		if err != nil || team.OrgID != inv.OrgID {
			return nil, authz.BadRequest("team is not in this org")
		}
	}
	// An email that already belongs to an AMP account is granted the invite's
	// role on the spot below, which makes this a grant path: a customer
	// account can't be invited as a plain member never mind owner/admin, so
	// refuse here rather than at acceptance (#3766). An email with no account
	// yet is checked when it is accepted.
	if u, err := s.Repo.GetUserByEmail(ctx, inv.Email); err == nil {
		if err := s.requireKindCanHoldOrgRole(ctx, u.ID, inv.OrgRole); err != nil {
			return nil, err
		}
	}
	inv.InvitedBy = p.UserRef()
	created, err := s.Repo.CreateInvite(ctx, inv)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "invite.create", OrgID: intRef(inv.OrgID), TargetType: "invite", TargetID: idStr(created.ID),
		Summary: "invited " + inv.Email + " as org " + inv.OrgRole, Details: map[string]any{"team_id": inv.TeamID, "team_role": inv.TeamRole}})
	if u, err := s.Repo.GetUserByEmail(ctx, inv.Email); err == nil {
		accepted, err := s.Repo.AcceptInvitesList(ctx, u.ID, u.Email)
		if err != nil {
			return nil, err
		}
		for _, a := range accepted {
			s.auditInviteAccept(ctx, p, u.ID, u.Email, a, "at once (the user already has an AMP account)")
		}
		return &MemberResult{Added: true, UserID: u.ID}, nil
	}
	return &MemberResult{Invited: true, Invite: created}, nil
}

func (s *Service) RevokeInvite(ctx context.Context, p authz.Principal, orgID, inviteID int) error {
	a, err := s.orgAccess(ctx, p, orgID)
	if err != nil {
		return err
	}
	if err := a.requireAdmin(orgID); err != nil {
		return err
	}
	inv, err := s.Repo.GetInvite(ctx, inviteID)
	if err != nil || inv.OrgID != orgID {
		return authz.NotFound("invite %d not found", inviteID)
	}
	if err := s.Repo.DeleteInvite(ctx, inviteID); err != nil {
		return err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "invite.revoke", OrgID: intRef(orgID), TargetType: "invite", TargetID: idStr(inviteID),
		Summary: "revoked the invite for " + inv.Email})
	return nil
}

// ---- Project ↔ team links ----

func (s *Service) ListProjectTeams(ctx context.Context, p authz.Principal, projectID int) ([]domain.ProjectTeam, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		return nil, err
	}
	return s.Repo.ListProjectTeams(ctx, projectID)
}

// SetProjectTeam links a team (of the project's org) as owner or member.
// Project owners, org admins and global admins may link any team in any
// role. A team's lead may link their own team as a member team, on a
// project they're already on.
func (s *Service) SetProjectTeam(ctx context.Context, p authz.Principal, projectID, teamID int, role string) ([]domain.ProjectTeam, error) {
	lvl, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member)
	if err != nil {
		return nil, err
	}
	if role == "" {
		role = domain.ProjectTeamMember
	}
	if role != domain.ProjectTeamOwner && role != domain.ProjectTeamMember {
		return nil, authz.BadRequest("role must be owner or member")
	}
	if lvl < authz.Owner {
		if role == domain.ProjectTeamOwner {
			return nil, authz.Forbidden("making a team an owner of the project takes a project owner or an org admin")
		}
		if err := s.requireLeadOfMemberLink(ctx, p, projectID, teamID); err != nil {
			return nil, err
		}
	}
	previous := s.projectTeamRole(ctx, projectID, teamID)
	if err := s.Repo.SetProjectTeam(ctx, projectID, teamID, role); err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "project.team.set", ProjectID: intRef(projectID), TargetType: "team", TargetID: idStr(teamID),
		Summary: "linked " + s.teamLabel(ctx, teamID) + " to the project as " + role, Details: map[string]any{"role": role, "previous": previous},
		Before: roleValue(previous), After: roleValue(role)})
	return s.Repo.ListProjectTeams(ctx, projectID)
}

// RemoveProjectTeam unlinks a team. Project owners and admins may unlink any
// team (but not the last owner team); a team's lead may unlink their own
// member team.
func (s *Service) RemoveProjectTeam(ctx context.Context, p authz.Principal, projectID, teamID int) ([]domain.ProjectTeam, error) {
	lvl, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member)
	if err != nil {
		return nil, err
	}
	if lvl < authz.Owner {
		if err := s.requireLeadOfMemberLink(ctx, p, projectID, teamID); err != nil {
			return nil, err
		}
	}
	previous := s.projectTeamRole(ctx, projectID, teamID)
	if err := s.Repo.RemoveProjectTeam(ctx, projectID, teamID); err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "project.team.remove", ProjectID: intRef(projectID), TargetType: "team", TargetID: idStr(teamID),
		Summary: "unlinked " + s.teamLabel(ctx, teamID) + " from the project", Details: map[string]any{"previous": previous},
		Before: roleValue(previous), After: roleValue("")})
	s.endRemoteAfterAccessChange(ctx, p) // the team's members, unless another link keeps them in
	return s.Repo.ListProjectTeams(ctx, projectID)
}

// requireLeadOfMemberLink allows a change to a team's link only for that
// team's lead, and never to an owner-team link.
func (s *Service) requireLeadOfMemberLink(ctx context.Context, p authz.Principal, projectID, teamID int) error {
	role, err := s.Repo.GetTeamRole(ctx, teamID, p.UserID)
	if err != nil {
		return err
	}
	if role != domain.TeamRoleLead {
		return authz.Forbidden("changing a project's teams takes a project owner, an org admin, or the lead of that team")
	}
	links, err := s.Repo.ListProjectTeams(ctx, projectID)
	if err != nil {
		return err
	}
	for _, l := range links {
		if l.TeamID == teamID && l.Role == domain.ProjectTeamOwner {
			return authz.Forbidden("%s is an owner team; changing it takes a project owner or an org admin", l.TeamName)
		}
	}
	return nil
}

// MoveProjectToOrg puts a project in an org (or moves it to another one).
// Links to teams outside the new org are removed, and ownerTeamID (a team
// in the new org, optional) becomes an owner team. Global admins may place
// any project, including one with no org yet; org owners and admins may
// move a project between orgs they administer.
func (s *Service) MoveProjectToOrg(ctx context.Context, p authz.Principal, projectID, orgID, ownerTeamID int) (*domain.Project, []domain.ProjectTeam, error) {
	proj, err := s.Repo.GetProject(ctx, projectID)
	if err != nil {
		return nil, nil, authz.NotFound("project %d not found", projectID)
	}
	if !p.Super {
		if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
			return nil, nil, err
		}
		if proj.OrgID == nil {
			return nil, nil, authz.Forbidden("only a global admin can place a project that isn't in an org yet")
		}
		if _, err := s.Authz.RequireOrgAdmin(ctx, p, *proj.OrgID); err != nil {
			return nil, nil, authz.Forbidden("moving a project out of its org takes an owner or admin of that org")
		}
	}
	if _, err := s.Authz.RequireOrgAdmin(ctx, p, orgID); err != nil {
		if p.Super {
			return nil, nil, authz.NotFound("org %d not found", orgID)
		}
		return nil, nil, authz.Forbidden("moving a project into an org takes an owner or admin of that org")
	}
	if proj.OrgID != nil && *proj.OrgID == orgID && ownerTeamID == 0 {
		return nil, nil, authz.BadRequest("the project is already in that org")
	}
	if err := s.guardReceiptsOrgMove(ctx, p, proj, orgID); err != nil {
		return nil, nil, err
	}
	removed, err := s.Repo.MoveProjectToOrg(ctx, projectID, orgID, ownerTeamID)
	if err != nil {
		return nil, nil, err
	}
	from := "no org"
	if proj.OrgID != nil {
		from = s.orgLabel(ctx, *proj.OrgID)
	}
	details := map[string]any{"from_org_id": proj.OrgID, "to_org_id": orgID, "removed_team_ids": teamIDs(removed)}
	summary := fmt.Sprintf("moved the project from %s to %s", from, s.orgLabel(ctx, orgID))
	if ownerTeamID != 0 {
		summary += "; " + s.teamLabel(ctx, ownerTeamID) + " is an owner team"
		details["owner_team_id"] = ownerTeamID
	}
	if len(removed) > 0 {
		summary += fmt.Sprintf("; unlinked %d team(s) from the old org", len(removed))
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "project.org.set", ProjectID: intRef(projectID), OrgID: intRef(orgID),
		TargetType: "project", TargetID: idStr(projectID), Summary: summary, Details: details})
	updated, err := s.Repo.GetProject(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	return updated, removed, nil
}

func teamIDs(ts []domain.ProjectTeam) []int {
	out := []int{}
	for _, t := range ts {
		out = append(out, t.TeamID)
	}
	return out
}

// Slugify turns a name into an org slug.
func Slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(name) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// userLabel names a user in audit summaries.
func (s *Service) userLabel(ctx context.Context, userID int) string {
	if u, err := s.Repo.GetUserByID(ctx, userID); err == nil {
		return u.Email
	}
	return "user " + idStr(userID)
}

func (s *Service) teamLabel(ctx context.Context, teamID int) string {
	if t, err := s.Repo.GetTeam(ctx, teamID); err == nil {
		return "team " + t.Name
	}
	return "team " + idStr(teamID)
}

func (s *Service) orgLabel(ctx context.Context, orgID int) string {
	if o, err := s.Repo.GetOrg(ctx, orgID); err == nil {
		return "org " + o.Name
	}
	return "org " + idStr(orgID)
}

func roleChange(previous string) string {
	if previous == "" {
		return " (added)"
	}
	return " (was " + previous + ")"
}

// roleValue is a membership role as an audit before/after value; "" (no
// membership) is {"role": null}.
func roleValue(role string) map[string]any {
	if role == "" {
		return map[string]any{"role": nil}
	}
	return map[string]any{"role": role}
}

// projectTeamRole is teamID's role on projectID ("" when not linked, or
// when the links can't be read).
func (s *Service) projectTeamRole(ctx context.Context, projectID, teamID int) string {
	links, err := s.Repo.ListProjectTeams(ctx, projectID)
	if err != nil {
		return ""
	}
	for _, l := range links {
		if l.TeamID == teamID {
			return l.Role
		}
	}
	return ""
}
