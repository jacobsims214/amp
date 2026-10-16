package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// Org and team administration.
//
//	GET    /api/orgs                       orgs you're in (global admins: all)
//	POST   /api/orgs                       {name, slug?, owner?}       global admin only
//	GET    /api/orgs/:id                   org + your role
//	PATCH  /api/orgs/:id                   {name}                      org admin
//	DELETE /api/orgs/:id                                               global admin, no projects left
//	GET    /api/orgs/:id/members
//	POST   /api/orgs/:id/members           {user, role}                org admin; unknown email → invite
//	PUT    /api/orgs/:id/members/:userId   {role}                      org admin
//	DELETE /api/orgs/:id/members/:userId                               org admin
//	GET    /api/orgs/:id/teams
//	POST   /api/orgs/:id/teams             {name}                      org admin
//	GET    /api/orgs/:id/invites                                       org admin
//	POST   /api/orgs/:id/invites           {email, org_role?, team_id?, team_role?}
//	DELETE /api/orgs/:id/invites/:inviteId                             org admin
//	GET    /api/teams/:id                  team + members
//	PATCH  /api/teams/:id                  {name}                      org admin
//	DELETE /api/teams/:id                                              org admin
//	POST   /api/teams/:id/members          {user, role}                team lead or org admin; unknown email → invite
//	PUT    /api/teams/:id/members/:userId  {role}
//	DELETE /api/teams/:id/members/:userId
//
// Only org owners (or global admins) can grant, change, or remove the org
// owner role.

var validOrgRoles = map[string]bool{domain.OrgRoleOwner: true, domain.OrgRoleAdmin: true, domain.OrgRoleMember: true}
var validTeamRoles = map[string]bool{domain.TeamRoleLead: true, domain.TeamRoleMember: true}

func (h *RestHandler) orgs(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		var uid *int
		if !pr.Super {
			uid = pr.UserRef()
		}
		orgs, err := h.repo.ListOrgs(r.Context(), uid)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		if pr.Super && pr.HasUser() {
			// ListOrgs(nil) has no "my role"; fill it in for the admin.
			mine, err := h.repo.ListOrgs(r.Context(), pr.UserRef())
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			roles := map[int]string{}
			for _, o := range mine {
				roles[o.ID] = o.MyRole
			}
			for i := range orgs {
				orgs[i].MyRole = roles[orgs[i].ID]
			}
		}
		jsonOK(w, map[string]interface{}{"orgs": orgs})

	case http.MethodPost:
		if !pr.Super {
			fail(w, authz.Forbidden("only global admins can create orgs"), 403)
			return
		}
		var body struct {
			Name  string `json:"name"`
			Slug  string `json:"slug"`
			Owner string `json:"owner"` // user id or email; defaults to you
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
			jsonErr(w, errorf("name is required"), 400)
			return
		}
		slug := body.Slug
		if slug == "" {
			slug = slugify(body.Name)
		}
		if !slugRe.MatchString(slug) {
			jsonErr(w, errorf("slug must be lowercase letters, digits and dashes"), 400)
			return
		}

		ownerID := pr.UserRef()
		var inviteOwner string
		if body.Owner != "" {
			id, err := h.svc.Authz.ResolveUser(r.Context(), pr, body.Owner)
			switch {
			case err == nil:
				ownerID = &id
			case strings.Contains(body.Owner, "@"):
				inviteOwner = body.Owner // hasn't signed in yet: invite as owner
			default:
				fail(w, err, 400)
				return
			}
		}
		org, err := h.repo.CreateOrg(r.Context(), strings.TrimSpace(body.Name), slug, pr.UserRef(), ownerID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		resp := map[string]interface{}{"org": org}
		if inviteOwner != "" {
			inv, err := h.repo.CreateInvite(r.Context(), domain.Invite{
				Email: inviteOwner, OrgID: org.ID, OrgRole: domain.OrgRoleOwner, TeamRole: domain.TeamRoleMember, InvitedBy: pr.UserRef(),
			})
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			resp["owner_invite"] = inv
		}
		w.WriteHeader(http.StatusCreated)
		jsonOK(w, resp)

	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (h *RestHandler) orgSub(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path[len("/api/orgs/"):])
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	orgID, err := strconv.Atoi(parts[0])
	if err != nil {
		jsonErr(w, errorf("invalid org id"), 400)
		return
	}
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	role, err := h.svc.Authz.RequireOrgMember(ctx, pr, orgID)
	if err != nil {
		fail(w, err, 404)
		return
	}
	orgAdmin := pr.Super || role == domain.OrgRoleOwner || role == domain.OrgRoleAdmin
	orgOwner := pr.Super || role == domain.OrgRoleOwner
	needAdmin := func() bool {
		if !orgAdmin {
			fail(w, authz.Forbidden("org %d: requires org owner or admin", orgID), 403)
		}
		return orgAdmin
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			org, err := h.repo.GetOrg(ctx, orgID)
			if err != nil {
				fail(w, err, 404)
				return
			}
			org.MyRole = role
			jsonOK(w, map[string]interface{}{"org": org})
		case http.MethodPatch:
			if !needAdmin() {
				return
			}
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
				jsonErr(w, errorf("name is required"), 400)
				return
			}
			if err := h.repo.RenameOrg(ctx, orgID, strings.TrimSpace(body.Name)); err != nil {
				fail(w, err, 500)
				return
			}
			org, _ := h.repo.GetOrg(ctx, orgID)
			jsonOK(w, map[string]interface{}{"org": org})
		case http.MethodDelete:
			if !pr.Super {
				fail(w, authz.Forbidden("only global admins can delete orgs"), 403)
				return
			}
			if err := h.repo.DeleteOrg(ctx, orgID); err != nil {
				fail(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"org_id": orgID, "deleted": true})
		default:
			http.Error(w, "method not allowed", 405)
		}
		return
	}

	switch parts[1] {
	case "members":
		if len(parts) == 2 && r.Method == http.MethodGet {
			members, err := h.repo.ListOrgMembers(ctx, orgID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"members": members})
			return
		}
		if !needAdmin() {
			return
		}
		switch {
		case len(parts) == 2 && r.Method == http.MethodPost, len(parts) == 3 && r.Method == http.MethodPut:
			var body struct {
				User string `json:"user"`
				Role string `json:"role"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, err, 400)
				return
			}
			if len(parts) == 3 {
				body.User = parts[2]
			}
			if body.Role == "" {
				body.Role = domain.OrgRoleMember
			}
			if !validOrgRoles[body.Role] {
				jsonErr(w, errorf("role must be owner, admin, or member"), 400)
				return
			}
			userID, err := h.svc.Authz.ResolveUser(ctx, pr, body.User)
			if err != nil {
				if r.Method == http.MethodPost && strings.Contains(body.User, "@") {
					if body.Role == domain.OrgRoleOwner && !orgOwner {
						fail(w, authz.Forbidden("only org owners can grant the owner role"), 403)
						return
					}
					h.invite(w, r, pr, domain.Invite{Email: body.User, OrgID: orgID, OrgRole: body.Role, TeamRole: domain.TeamRoleMember})
					return
				}
				fail(w, err, 400)
				return
			}
			current, err := h.repo.GetOrgRole(ctx, orgID, userID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			if (body.Role == domain.OrgRoleOwner || current == domain.OrgRoleOwner) && !orgOwner {
				fail(w, authz.Forbidden("only org owners can grant, change, or remove the owner role"), 403)
				return
			}
			if err := h.repo.SetOrgMember(ctx, orgID, userID, body.Role); err != nil {
				fail(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"org_id": orgID, "user_id": userID, "role": body.Role})

		case len(parts) == 3 && r.Method == http.MethodDelete:
			userID, err := strconv.Atoi(parts[2])
			if err != nil {
				jsonErr(w, errorf("invalid user id"), 400)
				return
			}
			current, err := h.repo.GetOrgRole(ctx, orgID, userID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			if current == domain.OrgRoleOwner && !orgOwner {
				fail(w, authz.Forbidden("only org owners can remove an owner"), 403)
				return
			}
			if err := h.repo.RemoveOrgMember(ctx, orgID, userID); err != nil {
				fail(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"org_id": orgID, "user_id": userID, "removed": true})
		default:
			http.Error(w, "method not allowed", 405)
		}

	case "teams":
		switch r.Method {
		case http.MethodGet:
			teams, err := h.repo.ListTeams(ctx, orgID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"teams": teams})
		case http.MethodPost:
			if !needAdmin() {
				return
			}
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
				jsonErr(w, errorf("name is required"), 400)
				return
			}
			team, err := h.repo.CreateTeam(ctx, orgID, strings.TrimSpace(body.Name), pr.UserRef())
			if err != nil {
				fail(w, err, 500)
				return
			}
			w.WriteHeader(http.StatusCreated)
			jsonOK(w, map[string]interface{}{"team": team})
		default:
			http.Error(w, "method not allowed", 405)
		}

	case "invites":
		switch {
		case len(parts) == 2 && r.Method == http.MethodGet:
			if !needAdmin() {
				return
			}
			invites, err := h.repo.ListPendingInvites(ctx, orgID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"invites": invites})
		case len(parts) == 2 && r.Method == http.MethodPost:
			var inv domain.Invite
			if err := json.NewDecoder(r.Body).Decode(&inv); err != nil {
				jsonErr(w, err, 400)
				return
			}
			inv.OrgID = orgID
			if inv.OrgRole == "" {
				inv.OrgRole = domain.OrgRoleMember
			}
			if inv.TeamRole == "" {
				inv.TeamRole = domain.TeamRoleMember
			}
			// Team leads may invite into their own team as plain org members.
			if !orgAdmin {
				if inv.TeamID == nil || inv.OrgRole != domain.OrgRoleMember {
					fail(w, authz.Forbidden("org %d: requires org owner or admin", orgID), 403)
					return
				}
				team, err := h.repo.GetTeam(ctx, *inv.TeamID)
				if err != nil || team.OrgID != orgID {
					jsonErr(w, errorf("team is not in this org"), 400)
					return
				}
				if err := h.svc.Authz.RequireTeamManager(ctx, pr, team); err != nil {
					fail(w, err, 403)
					return
				}
			}
			if inv.OrgRole == domain.OrgRoleOwner && !orgOwner {
				fail(w, authz.Forbidden("only org owners can grant the owner role"), 403)
				return
			}
			h.invite(w, r, pr, inv)
		case len(parts) == 3 && r.Method == http.MethodDelete:
			if !needAdmin() {
				return
			}
			id, err := strconv.Atoi(parts[2])
			if err != nil {
				jsonErr(w, errorf("invalid invite id"), 400)
				return
			}
			inv, err := h.repo.GetInvite(ctx, id)
			if err != nil || inv.OrgID != orgID {
				jsonErr(w, errorf("invite not found"), 404)
				return
			}
			if err := h.repo.DeleteInvite(ctx, id); err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"invite_id": id, "revoked": true})
		default:
			http.Error(w, "method not allowed", 405)
		}

	default:
		http.NotFound(w, r)
	}
}

// invite validates and records a pending membership. If the email already
// belongs to an AMP user, the membership is applied right away instead.
func (h *RestHandler) invite(w http.ResponseWriter, r *http.Request, pr authz.Principal, inv domain.Invite) {
	ctx := r.Context()
	inv.Email = strings.ToLower(strings.TrimSpace(inv.Email))
	if !strings.Contains(inv.Email, "@") {
		jsonErr(w, errorf("a valid email is required"), 400)
		return
	}
	if !validOrgRoles[inv.OrgRole] || !validTeamRoles[inv.TeamRole] {
		jsonErr(w, errorf("invalid org_role or team_role"), 400)
		return
	}
	if inv.TeamID != nil {
		team, err := h.repo.GetTeam(ctx, *inv.TeamID)
		if err != nil || team.OrgID != inv.OrgID {
			jsonErr(w, errorf("team is not in this org"), 400)
			return
		}
	}
	inv.InvitedBy = pr.UserRef()
	created, err := h.repo.CreateInvite(ctx, inv)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	// Already a user: apply now rather than waiting for their next sign-in.
	if u, err := h.repo.GetUserByEmail(ctx, inv.Email); err == nil {
		if _, err := h.repo.AcceptInvites(ctx, u.ID, u.Email); err != nil {
			jsonErr(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"added": true, "user_id": u.ID})
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]interface{}{"invited": true, "invite": created})
}

func (h *RestHandler) teamSub(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path[len("/api/teams/"):])
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	teamID, err := strconv.Atoi(parts[0])
	if err != nil {
		jsonErr(w, errorf("invalid team id"), 400)
		return
	}
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	team, err := h.repo.GetTeam(ctx, teamID)
	if err != nil {
		fail(w, err, 404)
		return
	}
	role, err := h.svc.Authz.RequireOrgMember(ctx, pr, team.OrgID)
	if err != nil {
		fail(w, authz.NotFound("team %d not found", teamID), 404)
		return
	}
	orgAdmin := pr.Super || role == domain.OrgRoleOwner || role == domain.OrgRoleAdmin

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			members, err := h.repo.ListTeamMembers(ctx, teamID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"team": team, "members": members})
		case http.MethodPatch, http.MethodDelete:
			if !orgAdmin {
				fail(w, authz.Forbidden("team %d: requires org owner or admin", teamID), 403)
				return
			}
			if r.Method == http.MethodDelete {
				if err := h.repo.DeleteTeam(ctx, teamID); err != nil {
					fail(w, err, 500)
					return
				}
				jsonOK(w, map[string]interface{}{"team_id": teamID, "deleted": true})
				return
			}
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
				jsonErr(w, errorf("name is required"), 400)
				return
			}
			if err := h.repo.RenameTeam(ctx, teamID, strings.TrimSpace(body.Name)); err != nil {
				fail(w, err, 500)
				return
			}
			team, _ = h.repo.GetTeam(ctx, teamID)
			jsonOK(w, map[string]interface{}{"team": team})
		default:
			http.Error(w, "method not allowed", 405)
		}
		return
	}

	if parts[1] != "members" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodGet {
		members, err := h.repo.ListTeamMembers(ctx, teamID)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"members": members})
		return
	}
	if err := h.svc.Authz.RequireTeamManager(ctx, pr, team); err != nil {
		fail(w, err, 403)
		return
	}
	switch {
	case len(parts) == 2 && r.Method == http.MethodPost, len(parts) == 3 && r.Method == http.MethodPut:
		var body struct {
			User string `json:"user"`
			Role string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		if len(parts) == 3 {
			body.User = parts[2]
		}
		if body.Role == "" {
			body.Role = domain.TeamRoleMember
		}
		if !validTeamRoles[body.Role] {
			jsonErr(w, errorf("role must be lead or member"), 400)
			return
		}
		userID, err := h.svc.Authz.ResolveUser(ctx, pr, body.User)
		if err != nil {
			if r.Method == http.MethodPost && strings.Contains(body.User, "@") {
				h.invite(w, r, pr, domain.Invite{Email: body.User, OrgID: team.OrgID, OrgRole: domain.OrgRoleMember, TeamID: &team.ID, TeamRole: body.Role})
				return
			}
			fail(w, err, 400)
			return
		}
		if err := h.repo.SetTeamMember(ctx, teamID, userID, body.Role); err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"team_id": teamID, "user_id": userID, "role": body.Role})
	case len(parts) == 3 && r.Method == http.MethodDelete:
		userID, err := strconv.Atoi(parts[2])
		if err != nil {
			jsonErr(w, errorf("invalid user id"), 400)
			return
		}
		if err := h.repo.RemoveTeamMember(ctx, teamID, userID); err != nil {
			jsonErr(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"team_id": teamID, "user_id": userID, "removed": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// projectTeams serves /api/projects/:id/teams[/:teamId]. Reading needs
// project access; changing links needs project owner access.
func (h *RestHandler) projectTeams(w http.ResponseWriter, r *http.Request, pr authz.Principal, projectID int, lvl authz.Level, rest []string) {
	ctx := r.Context()
	if len(rest) == 0 && r.Method == http.MethodGet {
		teams, err := h.repo.ListProjectTeams(ctx, projectID)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"teams": teams})
		return
	}
	if len(rest) != 1 {
		http.NotFound(w, r)
		return
	}
	if lvl < authz.Owner {
		fail(w, authz.Forbidden("changing a project's teams requires project owner access"), 403)
		return
	}
	teamID, err := strconv.Atoi(rest[0])
	if err != nil {
		jsonErr(w, errorf("invalid team id"), 400)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Role string `json:"role"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Role == "" {
			body.Role = domain.ProjectTeamMember
		}
		if body.Role != domain.ProjectTeamOwner && body.Role != domain.ProjectTeamMember {
			jsonErr(w, errorf("role must be owner or member"), 400)
			return
		}
		if err := h.repo.SetProjectTeam(ctx, projectID, teamID, body.Role); err != nil {
			fail(w, err, 500)
			return
		}
	case http.MethodDelete:
		if err := h.repo.RemoveProjectTeam(ctx, projectID, teamID); err != nil {
			fail(w, err, 500)
			return
		}
	default:
		http.Error(w, "method not allowed", 405)
		return
	}
	teams, err := h.repo.ListProjectTeams(ctx, projectID)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	jsonOK(w, map[string]interface{}{"teams": teams})
}

// groupingOwners serves POST /api/{epics,stories}/:id/owners {user} and
// DELETE /api/{epics,stories}/:id/owners/:userId. The caller's edit rights
// were checked by the route.
func (h *RestHandler) groupingOwners(w http.ResponseWriter, r *http.Request, pr authz.Principal, kind string, id, projectID int, rest []string) {
	ctx := r.Context()
	var ids []int
	switch {
	case len(rest) == 0 && r.Method == http.MethodPost:
		var body struct {
			User string `json:"user"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		resolved, err := h.svc.ResolveOwnerRefs(ctx, pr, projectID, []string{body.User})
		if err != nil {
			fail(w, err, 400)
			return
		}
		ids = resolved
		if kind == "epic" {
			err = h.repo.AddEpicOwners(ctx, id, ids)
		} else {
			err = h.repo.AddStoryOwners(ctx, id, ids)
		}
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
	case len(rest) == 1 && r.Method == http.MethodDelete:
		uid, err := strconv.Atoi(rest[0])
		if err != nil {
			jsonErr(w, errorf("invalid user id"), 400)
			return
		}
		if kind == "epic" {
			err = h.repo.RemoveEpicOwners(ctx, id, []int{uid})
		} else {
			err = h.repo.RemoveStoryOwners(ctx, id, []int{uid})
		}
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
	default:
		http.Error(w, "method not allowed", 405)
		return
	}
	if kind == "epic" {
		e, err := h.repo.GetEpic(ctx, id)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		h.hub.Publish(domain.Event{Type: domain.EventEpicUpdated, ProjectID: projectID, Payload: e, At: time.Now()})
		jsonOK(w, map[string]interface{}{"epic": e})
		return
	}
	st, err := h.repo.GetStory(ctx, id)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	h.hub.Publish(domain.Event{Type: domain.EventStoryUpdated, ProjectID: projectID, Payload: st, At: time.Now()})
	jsonOK(w, map[string]interface{}{"story": st})
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func slugify(name string) string {
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
