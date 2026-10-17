package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/kb"
)

// Knowledge bases come in three scopes: a project's (the default), a
// team's, and an org's. These rules are shared by REST and MCP; see
// docs/kb-scopes-design.md.
//
//	project: project members read and write
//	team:    members of the team read and write (org admins too only if on it)
//	org:     org members read and annotate; owners and admins write

// KBRef is how a caller names a knowledge base: a scope plus whichever ids
// it has. An empty Scope is inferred from the ids, project first.
type KBRef struct {
	Scope     string
	ProjectID int
	TeamID    int
	OrgID     int
}

// KB is a resolved knowledge base and what the caller may do in it.
type KB struct {
	Scope kb.Scope `json:"-"`
	Kind  string   `json:"scope"`
	ID    int      `json:"scope_id"`
	Name  string   `json:"name"`
	// CanRead covers reading, searching, and annotating.
	CanRead bool `json:"can_read"`
	// CanWrite covers writing, deleting, and reindexing.
	CanWrite bool `json:"can_write"`
}

func newKB(sc kb.Scope, name string, read, write bool) *KB {
	return &KB{Scope: sc, Kind: sc.Kind, ID: sc.ID, Name: name, CanRead: read, CanWrite: write}
}

// RequireWrite fails unless the caller may write to k.
func (k *KB) RequireWrite() error {
	if k.CanWrite {
		return nil
	}
	if k.Kind == kb.ScopeGlobal {
		return authz.Forbidden("the AMP guide is read-only: it ships with AMP from the kb/ folder of the AMP repo, so change it there")
	}
	if k.Kind == kb.ScopeOrg {
		return authz.Forbidden("only org owners and admins can change the %s org KB; annotate the doc instead (amp_kb_annotate)", k.Name)
	}
	return authz.Forbidden("you can't change the %s KB", k.Name)
}

// ResolveKB works out which knowledge base ref names and what p may do in
// it. It refuses rather than guesses when a team or org is ambiguous, and
// never crosses orgs: with a project, team and org ids must be in that
// project's org.
func (s *Service) ResolveKB(ctx context.Context, p authz.Principal, ref KBRef) (*KB, error) {
	kind := strings.ToLower(strings.TrimSpace(ref.Scope))
	if kind == kb.ScopeGlobal {
		// The AMP guide: the same for every user, holds nothing org-specific.
		return newKB(kb.Global(), "AMP guide", true, false), nil
	}
	if kind == "" {
		switch {
		case ref.ProjectID > 0:
			kind = kb.ScopeProject
		case ref.TeamID > 0:
			kind = kb.ScopeTeam
		case ref.OrgID > 0:
			kind = kb.ScopeOrg
		default:
			return nil, authz.BadRequest("name a knowledge base: project_id, team_id, or org_id")
		}
	}

	var proj *domain.Project
	if ref.ProjectID > 0 {
		if _, err := s.Authz.RequireProject(ctx, p, ref.ProjectID, authz.Member); err != nil {
			return nil, err
		}
		var err error
		if proj, err = s.Repo.GetProject(ctx, ref.ProjectID); err != nil {
			return nil, authz.NotFound("project %d not found", ref.ProjectID)
		}
	}

	switch kind {
	case kb.ScopeProject:
		if proj == nil {
			return nil, authz.BadRequest("project_id is required for the project KB")
		}
		return newKB(kb.Project(proj.ID), "project "+proj.Name, true, true), nil
	case kb.ScopeOrg:
		return s.resolveOrgKB(ctx, p, proj, ref.OrgID)
	case kb.ScopeTeam:
		return s.resolveTeamKB(ctx, p, proj, ref.TeamID)
	}
	return nil, authz.BadRequest("scope must be project, team, org, or global")
}

func (s *Service) resolveOrgKB(ctx context.Context, p authz.Principal, proj *domain.Project, orgID int) (*KB, error) {
	if proj != nil {
		if proj.OrgID == nil {
			return nil, authz.BadRequest("project %d isn't in an org", proj.ID)
		}
		if orgID != 0 && orgID != *proj.OrgID {
			return nil, authz.BadRequest("org %d isn't project %d's org; a project's org KB is its own org's (org %d)", orgID, proj.ID, *proj.OrgID)
		}
		orgID = *proj.OrgID
	}
	if orgID == 0 {
		orgs, err := s.Repo.ListOrgs(ctx, p.UserRef())
		if err != nil {
			return nil, err
		}
		if len(orgs) != 1 {
			return nil, authz.BadRequest("pass org_id or project_id; you're in %s", orgList(orgs))
		}
		orgID = orgs[0].ID
	}
	role, err := s.Authz.RequireOrgMember(ctx, p, orgID)
	if err != nil {
		return nil, err
	}
	org, err := s.Repo.GetOrg(ctx, orgID)
	if err != nil {
		return nil, authz.NotFound("org %d not found", orgID)
	}
	admin := p.Super || role == domain.OrgRoleOwner || role == domain.OrgRoleAdmin
	return newKB(kb.Org(org.ID), org.Name, true, admin), nil
}

func (s *Service) resolveTeamKB(ctx context.Context, p authz.Principal, proj *domain.Project, teamID int) (*KB, error) {
	if teamID == 0 {
		candidates, err := s.myTeamsFor(ctx, p, proj)
		if err != nil {
			return nil, err
		}
		if len(candidates) != 1 {
			return nil, candidates.ambiguity(proj)
		}
		teamID = candidates[0].id
	}
	team, err := s.Repo.GetTeam(ctx, teamID)
	if err != nil {
		return nil, authz.NotFound("team %d not found", teamID)
	}
	if proj != nil && proj.OrgID != nil && team.OrgID != *proj.OrgID {
		return nil, authz.BadRequest("team %d is in a different org than project %d", teamID, proj.ID)
	}
	if p.Super {
		return newKB(kb.Team(team.ID), team.Name, true, true), nil
	}
	role, err := s.Repo.GetTeamRole(ctx, team.ID, p.UserID)
	if err != nil {
		return nil, err
	}
	if role != "" {
		return newKB(kb.Team(team.ID), team.Name, true, true), nil
	}
	// Teams are visible to their org; to anyone else this one doesn't exist.
	if orgRole, err := s.Authz.OrgRole(ctx, p, team.OrgID); err != nil || orgRole == "" {
		return nil, authz.NotFound("team %d not found", teamID)
	}
	return nil, authz.Forbidden("the %s team KB is for its members; ask a team lead or org admin to add you to the team", team.Name)
}

type teamChoice struct {
	id   int
	name string
	role string // the team's link to the project, when there is one
}

type teamChoices []teamChoice

// myTeamsFor lists the teams p could mean by "my team": with a project,
// p's teams linked to it; without one, all of p's teams.
func (s *Service) myTeamsFor(ctx context.Context, p authz.Principal, proj *domain.Project) (teamChoices, error) {
	var out teamChoices
	if proj != nil {
		links, err := s.Repo.ListProjectTeams(ctx, proj.ID)
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			role, err := s.Repo.GetTeamRole(ctx, l.TeamID, p.UserID)
			if err != nil {
				return nil, err
			}
			if role != "" || p.Super {
				out = append(out, teamChoice{id: l.TeamID, name: l.TeamName, role: l.Role})
			}
		}
		if len(out) == 0 {
			for _, l := range links {
				out = append(out, teamChoice{id: l.TeamID, name: l.TeamName, role: l.Role})
			}
			return nil, authz.Forbidden("you're not on any of project %d's teams (%s); ask a team lead or org admin to add you", proj.ID, out.String())
		}
		return out, nil
	}
	orgs, err := s.Repo.ListOrgs(ctx, p.UserRef())
	if err != nil {
		return nil, err
	}
	for _, o := range orgs {
		teams, err := s.Repo.ListUserTeams(ctx, o.ID, p.UserID)
		if err != nil {
			return nil, err
		}
		for _, t := range teams { // ListUserTeams: UserID is the team id, Email its name
			out = append(out, teamChoice{id: t.UserID, name: t.Email})
		}
	}
	return out, nil
}

func (c teamChoices) ambiguity(proj *domain.Project) error {
	if len(c) == 0 {
		return authz.BadRequest("you're not on any team; pass team_id")
	}
	if proj != nil {
		return authz.BadRequest("project %d has several of your teams (%s); pass team_id", proj.ID, c.String())
	}
	return authz.BadRequest("you're on several teams (%s); pass team_id or project_id", c.String())
}

func (c teamChoices) String() string {
	const max = 10
	parts := make([]string, 0, len(c))
	for i, t := range c {
		if i == max {
			parts = append(parts, fmt.Sprintf("and %d more", len(c)-max))
			break
		}
		if t.role != "" {
			parts = append(parts, fmt.Sprintf("%s (%s, team_id %d)", t.name, t.role, t.id))
		} else {
			parts = append(parts, fmt.Sprintf("%s (team_id %d)", t.name, t.id))
		}
	}
	return strings.Join(parts, ", ")
}

func orgList(orgs []domain.Org) string {
	if len(orgs) == 0 {
		return "no orgs"
	}
	const max = 10
	parts := make([]string, 0, len(orgs))
	for i, o := range orgs {
		if i == max {
			parts = append(parts, fmt.Sprintf("and %d more", len(orgs)-max))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (org_id %d)", o.Name, o.ID))
	}
	return strings.Join(parts, ", ")
}

// KBSource is one knowledge base available around a project.
type KBSource struct {
	KB
	// TeamRole is how a team is linked to the project: owner or member.
	TeamRole string `json:"team_role,omitempty"`
	// Docs is the number of documents, when the caller can read the KB.
	Docs *int `json:"docs,omitempty"`
}

// KBSources lists the knowledge bases around a project: the project's own,
// its org's, and each linked team's (owner teams first), with what p may do
// in each. Teams p isn't on are listed with can_read false and no contents,
// so an agent can tell its user the KB exists.
func (s *Service) KBSources(ctx context.Context, p authz.Principal, projectID int, count func(kb.Scope) int) ([]KBSource, error) {
	home, err := s.ResolveKB(ctx, p, KBRef{Scope: kb.ScopeProject, ProjectID: projectID})
	if err != nil {
		return nil, err
	}
	withDocs := func(k KB, teamRole string) KBSource {
		src := KBSource{KB: k, TeamRole: teamRole}
		if k.CanRead && count != nil {
			n := count(k.Scope)
			src.Docs = &n
		}
		return src
	}
	out := []KBSource{withDocs(*home, "")}

	if org, err := s.ResolveKB(ctx, p, KBRef{Scope: kb.ScopeOrg, ProjectID: projectID}); err == nil {
		out = append(out, withDocs(*org, ""))
	}

	links, err := s.Repo.ListProjectTeams(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, ownersFirst := range []bool{true, false} {
		for _, l := range links {
			if (l.Role == domain.ProjectTeamOwner) != ownersFirst {
				continue
			}
			team, err := s.ResolveKB(ctx, p, KBRef{Scope: kb.ScopeTeam, ProjectID: projectID, TeamID: l.TeamID})
			if err != nil {
				// Not on the team: say it exists, nothing more.
				out = append(out, KBSource{KB: KB{Kind: kb.ScopeTeam, ID: l.TeamID, Name: l.TeamName}, TeamRole: l.Role})
				continue
			}
			out = append(out, withDocs(*team, l.Role))
		}
	}
	out = append(out, withDocs(*newKB(kb.Global(), "AMP guide", true, false), ""))
	return out, nil
}
