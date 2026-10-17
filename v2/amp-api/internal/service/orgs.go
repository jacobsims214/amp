package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/kb"
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
			ownerID = &id
		case strings.Contains(owner, "@"):
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
	if err := s.Repo.RenameOrg(ctx, orgID, strings.TrimSpace(name)); err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "org.rename", OrgID: intRef(orgID), TargetType: "org", TargetID: idStr(orgID),
		Summary: "renamed org to " + strings.TrimSpace(name)})
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
	// The org's teams went with it; so do its and their knowledge bases.
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
		Summary: "set " + s.userLabel(ctx, userID) + " to org " + role + roleChange(current), Details: map[string]any{"role": role, "previous": current}})
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
		Summary: "removed " + s.userLabel(ctx, userID) + " from the org (was " + current + ")", Details: map[string]any{"previous": current}})
	return userID, nil
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
	if err := s.Repo.SetTeamMember(ctx, teamID, userID, role); err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "team.member.set", OrgID: intRef(team.OrgID), TargetType: "team", TargetID: idStr(teamID),
		Summary: "set " + s.userLabel(ctx, userID) + " to " + role + " of team " + team.Name, Details: map[string]any{"user_id": userID, "role": role}})
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
	if err := s.Repo.RemoveTeamMember(ctx, teamID, userID); err != nil {
		return 0, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "team.member.remove", OrgID: intRef(team.OrgID), TargetType: "team", TargetID: idStr(teamID),
		Summary: "removed " + s.userLabel(ctx, userID) + " from team " + team.Name, Details: map[string]any{"user_id": userID}})
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

// Invite records a pending membership. Org admins can invite with any role
// (owner only by owners); team leads may invite into their own team as
// plain org members.
func (s *Service) Invite(ctx context.Context, p authz.Principal, inv domain.Invite) (*MemberResult, error) {
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
	inv.InvitedBy = p.UserRef()
	created, err := s.Repo.CreateInvite(ctx, inv)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "invite.create", OrgID: intRef(inv.OrgID), TargetType: "invite", TargetID: idStr(created.ID),
		Summary: "invited " + inv.Email + " as org " + inv.OrgRole, Details: map[string]any{"team_id": inv.TeamID, "team_role": inv.TeamRole}})
	if u, err := s.Repo.GetUserByEmail(ctx, inv.Email); err == nil {
		if _, err := s.Repo.AcceptInvites(ctx, u.ID, u.Email); err != nil {
			return nil, err
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
	if err := s.Repo.SetProjectTeam(ctx, projectID, teamID, role); err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "project.team.set", ProjectID: intRef(projectID), TargetType: "team", TargetID: idStr(teamID),
		Summary: "linked " + s.teamLabel(ctx, teamID) + " to the project as " + role, Details: map[string]any{"role": role}})
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
	if err := s.Repo.RemoveProjectTeam(ctx, projectID, teamID); err != nil {
		return nil, err
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "project.team.remove", ProjectID: intRef(projectID), TargetType: "team", TargetID: idStr(teamID),
		Summary: "unlinked " + s.teamLabel(ctx, teamID) + " from the project"})
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
