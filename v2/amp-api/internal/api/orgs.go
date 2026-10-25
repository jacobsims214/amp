package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// Org and team administration. The rules live in internal/service (shared
// with the MCP admin tools); these handlers only map HTTP to it.
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
//	GET    /api/orgs/:id/remote-control                                org admin (remote.go)
//	PUT    /api/orgs/:id/remote-control  {enabled, idle_timeout_min, device_pin_days}  org admin, web UI
//	GET    /api/teams/:id                  team + members
//	PATCH  /api/teams/:id                  {name}                      org admin
//	DELETE /api/teams/:id                                              org admin
//	POST   /api/teams/:id/members          {user, role}                team lead or org admin; unknown email → invite
//	PUT    /api/teams/:id/members/:userId  {role}
//	DELETE /api/teams/:id/members/:userId

func (h *RestHandler) orgs(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		orgs, err := h.svc.ListOrgs(r.Context(), pr)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"orgs": orgs})
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Slug  string `json:"slug"`
			Owner string `json:"owner"` // user id or email; defaults to you
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		org, inv, err := h.svc.CreateOrg(r.Context(), pr, body.Name, body.Slug, body.Owner)
		if err != nil {
			fail(w, err, 500)
			return
		}
		resp := map[string]interface{}{"org": org}
		if inv != nil {
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

	if len(parts) == 2 && parts[1] == "remote-control" {
		h.orgRemoteControl(w, r, pr, orgID)
		return
	}

	if len(parts) == 2 && parts[1] == "webhooks" {
		h.webhookCollection(w, r, pr, &orgID, nil)
		return
	}

	if len(parts) == 2 && parts[1] == "portfolio" {
		// GET ?since=&until= (RFC 3339; default the last 30 days) &split_by=<metadata key>:
		// the org's projects side by side, with token spend split by any metadata key.
		q := r.URL.Query()
		until, since := openUntil(), time.Now().Add(-30*24*time.Hour)
		for key, dst := range map[string]*time.Time{"since": &since, "until": &until} {
			if v := q.Get(key); v != "" {
				t, err := time.Parse(time.RFC3339, v)
				if err != nil {
					jsonErr(w, fmt.Errorf("%s: %w", key, err), 400)
					return
				}
				*dst = t
			}
		}
		pf, err := h.svc.OrgPortfolio(ctx, pr, orgID, since, until, q.Get("split_by"))
		if err != nil {
			fail(w, err, 404)
			return
		}
		jsonOK(w, pf)
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			org, err := h.svc.GetOrg(ctx, pr, orgID)
			if err != nil {
				fail(w, err, 404)
				return
			}
			jsonOK(w, map[string]interface{}{"org": org})
		case http.MethodPatch:
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			org, err := h.svc.RenameOrg(ctx, pr, orgID, body.Name)
			if err != nil {
				fail(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"org": org})
		case http.MethodDelete:
			if err := h.svc.DeleteOrg(ctx, pr, orgID); err != nil {
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
		switch {
		case len(parts) == 2 && r.Method == http.MethodGet:
			members, err := h.svc.ListOrgMembers(ctx, pr, orgID)
			if err != nil {
				fail(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"members": members})
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
			res, err := h.svc.AddOrgMember(ctx, pr, orgID, body.User, body.Role, r.Method == http.MethodPost)
			memberResponse(w, res, err, map[string]interface{}{"org_id": orgID})
		case len(parts) == 3 && r.Method == http.MethodDelete:
			userID, err := h.svc.RemoveOrgMember(ctx, pr, orgID, parts[2])
			if err != nil {
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
			teams, err := h.svc.ListTeams(ctx, pr, orgID)
			if err != nil {
				fail(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"teams": teams})
		case http.MethodPost:
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			team, err := h.svc.CreateTeam(ctx, pr, orgID, body.Name)
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
			invites, err := h.svc.ListInvites(ctx, pr, orgID)
			if err != nil {
				fail(w, err, 500)
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
			res, err := h.svc.Invite(ctx, pr, inv)
			memberResponse(w, res, err, map[string]interface{}{"org_id": orgID})
		case len(parts) == 3 && r.Method == http.MethodDelete:
			id, err := strconv.Atoi(parts[2])
			if err != nil {
				jsonErr(w, errorf("invalid invite id"), 400)
				return
			}
			if err := h.svc.RevokeInvite(ctx, pr, orgID, id); err != nil {
				fail(w, err, 500)
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

// memberResponse writes the result of adding someone: 201 for a new invite,
// 200 when they were added directly.
func memberResponse(w http.ResponseWriter, res *service.MemberResult, err error, extra map[string]interface{}) {
	if err != nil {
		fail(w, err, 500)
		return
	}
	if res.Invited {
		w.WriteHeader(http.StatusCreated)
		jsonOK(w, map[string]interface{}{"invited": true, "invite": res.Invite})
		return
	}
	out := map[string]interface{}{"added": true, "user_id": res.UserID}
	if res.Role != "" {
		out["role"] = res.Role
	}
	for k, v := range extra {
		out[k] = v
	}
	jsonOK(w, out)
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

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			team, members, err := h.svc.GetTeam(ctx, pr, teamID)
			if err != nil {
				fail(w, err, 404)
				return
			}
			jsonOK(w, map[string]interface{}{"team": team, "members": members})
		case http.MethodPatch:
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			team, err := h.svc.RenameTeam(ctx, pr, teamID, body.Name)
			if err != nil {
				fail(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"team": team})
		case http.MethodDelete:
			if err := h.svc.DeleteTeam(ctx, pr, teamID); err != nil {
				fail(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"team_id": teamID, "deleted": true})
		default:
			http.Error(w, "method not allowed", 405)
		}
		return
	}

	if parts[1] != "members" {
		http.NotFound(w, r)
		return
	}
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		_, members, err := h.svc.GetTeam(ctx, pr, teamID)
		if err != nil {
			fail(w, err, 404)
			return
		}
		jsonOK(w, map[string]interface{}{"members": members})
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
		res, err := h.svc.AddTeamMember(ctx, pr, teamID, body.User, body.Role, r.Method == http.MethodPost)
		memberResponse(w, res, err, map[string]interface{}{"team_id": teamID})
	case len(parts) == 3 && r.Method == http.MethodDelete:
		userID, err := h.svc.RemoveTeamMember(ctx, pr, teamID, parts[2])
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"team_id": teamID, "user_id": userID, "removed": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// projectTeams serves /api/projects/:id/teams[/:teamId]. Reading needs
// project access; changing links needs project owner access.
func (h *RestHandler) projectTeams(w http.ResponseWriter, r *http.Request, pr authz.Principal, projectID int, _ authz.Level, rest []string) {
	ctx := r.Context()
	if len(rest) == 0 && r.Method == http.MethodGet {
		teams, err := h.svc.ListProjectTeams(ctx, pr, projectID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"teams": teams})
		return
	}
	if len(rest) != 1 {
		http.NotFound(w, r)
		return
	}
	teamID, err := strconv.Atoi(rest[0])
	if err != nil {
		jsonErr(w, errorf("invalid team id"), 400)
		return
	}
	var teams []domain.ProjectTeam
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Role string `json:"role"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		teams, err = h.svc.SetProjectTeam(ctx, pr, projectID, teamID, body.Role)
	case http.MethodDelete:
		teams, err = h.svc.RemoveProjectTeam(ctx, pr, projectID, teamID)
	default:
		http.Error(w, "method not allowed", 405)
		return
	}
	if err != nil {
		fail(w, err, 500)
		return
	}
	jsonOK(w, map[string]interface{}{"teams": teams})
}

// groupingOwners serves POST /api/{epics,stories}/:id/owners {user} and
// DELETE /api/{epics,stories}/:id/owners/:userId. The caller's edit rights
// were checked by the route.
func (h *RestHandler) groupingOwners(w http.ResponseWriter, r *http.Request, pr authz.Principal, kind string, id, projectID int, rest []string) {
	ctx := r.Context()
	switch {
	case len(rest) == 0 && r.Method == http.MethodPost:
		var body struct {
			User string `json:"user"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		ids, err := h.svc.ResolveOwnerRefs(ctx, pr, projectID, []string{body.User})
		if err != nil {
			fail(w, err, 400)
			return
		}
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
