package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// RestHandler serves the REST API consumed by the frontend.
//
// Routes:
//
//	GET  /api/projects
//	POST /api/projects
//	POST /api/projects/import
//	GET  /api/projects/:id
//	GET  /api/projects/:id/epics
//	GET  /api/projects/:id/tasks
//	POST /api/projects/:id/tasks
//	GET  /api/projects/:id/export
//	GET  /api/epics/:id
//	GET  /api/epics/:id/stories
//	GET  /api/stories/:id
//	GET  /api/tasks/:id
//	PATCH /api/tasks/:id
//	DELETE /api/tasks/:id
//	GET  /api/tasks/:id/comments
//	POST /api/tasks/:id/comments
//	GET  /api/tasks/:id/history
//	POST /api/tasks/:id/dispatch
//	POST /api/tasks/:id/start      (a person starts their human ticket)
//	POST /api/tasks/:id/complete
//	POST /api/tasks/:id/start_at
//	GET  /api/events              ← SSE stream (only events for projects you can see)
//
// Multi-user (see orgs.go for org/team administration):
//
//	GET  /api/projects/:id/members        users with access, and their level
//	GET  /api/projects/:id/teams          linked teams
//	PUT  /api/projects/:id/teams/:teamId  {role: owner|member}
//	DELETE /api/projects/:id/teams/:teamId
//	POST /api/tasks/:id/assign            {user: id|email|"me"|"none"}
//	POST /api/epics/:id/owners            {user}     DELETE /api/epics/:id/owners/:userId
//	POST /api/stories/:id/owners          {user}     DELETE /api/stories/:id/owners/:userId
//
// Every route checks the caller's access (internal/authz). GET
// /api/projects/:id/tasks returns the caller's own work by default;
// ?owner=unassigned|any|<user> widens it.
type RestHandler struct {
	registry *actor.Registry
	repo     *repository.Repo
	hub      *hub.Hub
	kb       *kb.Service
	svc      *service.Service
}

func NewRestHandler(registry *actor.Registry, repo *repository.Repo, h *hub.Hub, kbSvc *kb.Service) *RestHandler {
	return &RestHandler{registry: registry, repo: repo, hub: h, kb: kbSvc, svc: service.New(repo, registry, kbSvc)}
}

// principal resolves the caller, writing the error response if it can't.
func (h *RestHandler) principal(w http.ResponseWriter, r *http.Request) (authz.Principal, bool) {
	p, err := authz.FromContext(r.Context())
	if err != nil {
		fail(w, err, 401)
		return p, false
	}
	return p, true
}

// requireProject checks the caller has at least min on projectID.
func (h *RestHandler) requireProject(w http.ResponseWriter, r *http.Request, p authz.Principal, projectID int, min authz.Level) (authz.Level, bool) {
	lvl, err := h.svc.Authz.RequireProject(r.Context(), p, projectID, min)
	if err != nil {
		fail(w, err, 500)
		return lvl, false
	}
	return lvl, true
}

// projectAudit is an audit entry about a whole project.
func projectAudit(action string, projectID int, summary string) domain.AuditEntry {
	return domain.AuditEntry{Action: action, ProjectID: &projectID, TargetType: "project", TargetID: strconv.Itoa(projectID), Summary: summary}
}

// fail writes err with the status authz assigns it, or fallback.
func fail(w http.ResponseWriter, err error, fallback int) {
	jsonErr(w, err, authz.StatusOf(err, fallback))
}

func (h *RestHandler) events(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	h.hub.ServeFiltered(w, r, h.svc.EventFilter(p))
}

func (h *RestHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/projects", h.projects)
	mux.HandleFunc("/api/projects/", h.projectSub)
	mux.HandleFunc("/api/epics/", h.epicSub)
	mux.HandleFunc("/api/stories/", h.storySub)
	mux.HandleFunc("/api/tasks/", h.taskSub)
	mux.HandleFunc("/api/kb/", h.kbDoc)
	mux.HandleFunc("/api/events", h.events)
	mux.HandleFunc("/api/orgs", h.orgs)
	mux.HandleFunc("/api/orgs/", h.orgSub)
	mux.HandleFunc("/api/teams/", h.teamSub)
	mux.HandleFunc("/api/me", h.me)
	mux.HandleFunc("/api/users", h.users)
	mux.HandleFunc("/api/users/", h.userSub)
	mux.HandleFunc("/api/tokens/", h.tokenSub)
	mux.HandleFunc("/api/agents", h.agents)
	mux.HandleFunc("/api/audit", h.audit)
	mux.HandleFunc("/api/webhooks/", h.webhookSub)
	mux.HandleFunc("/api/day-reports", h.dayReports)
	mux.HandleFunc("/api/day-reports/", h.dayReports)
	mux.HandleFunc("/api/velocity", h.velocity)
	mux.HandleFunc("/api/my-work", h.myWork)
	mux.HandleFunc("/api/trace", h.trace)
	mux.HandleFunc("/api/trace/merged", h.traceMerged)
	mux.HandleFunc("/api/metadata/", h.metadata)
	mux.HandleFunc("/api/artifacts/", h.artifactSub)
}

// ---- /api/me ----
// Returns the caller's identity as resolved by the auth middleware.
// Always 200 — when auth is disabled (local dev) this returns the
// anonymous "local-dev" identity so the UI doesn't need special-casing.
func (h *RestHandler) me(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonErr(w, errorf("no identity on request"), 401)
		return
	}
	resp := map[string]interface{}{
		"subject": ident.Subject,
		"email":   ident.Email,
		"name":    ident.Name,
		"roles":   []string{},
	}
	if ident.User != nil {
		resp["roles"] = ident.User.Roles
		resp["user_id"] = ident.User.ID
		resp["kind"] = ident.User.Kind
		resp["via_token"] = ident.APIToken
		uid := ident.User.ID
		orgs, err := h.repo.ListOrgs(r.Context(), &uid)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		resp["orgs"] = orgs
		// The teams they're on, across their orgs.
		type myTeam struct {
			ID    int    `json:"id"`
			Name  string `json:"name"`
			OrgID int    `json:"org_id"`
			Role  string `json:"role"`
		}
		teams := []myTeam{}
		for _, o := range orgs {
			list, err := h.repo.ListUserTeams(r.Context(), o.ID, uid)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			for _, t := range list { // ListUserTeams packs team id/name into UserID/Email
				teams = append(teams, myTeam{ID: t.UserID, Name: t.Email, OrgID: o.ID, Role: t.Role})
			}
		}
		resp["teams"] = teams
	}
	if ident.LocalDev {
		resp["roles"] = []string{domain.RoleAdmin}
	}
	jsonOK(w, resp)
}

// ---- /api/users (admin only) ----

func (h *RestHandler) users(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		auth.RequireRole(domain.RoleAdmin, func(w http.ResponseWriter, r *http.Request) {
			users, err := h.repo.ListUsers(r.Context())
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"users": users})
		})(w, r)
	default:
		http.NotFound(w, r)
	}
}

// /api/users/:id/role         PATCH  { "role": "admin", "grant": true }   (global admins)
// /api/users/:id              DELETE                                    (global admins)
// /api/users/:id/deactivate   POST   (global admins; managers for their agents)
// /api/users/:id/reactivate   POST
// /api/users/:id/tokens       GET, POST { name, expires_in_days }  (:id may be "me")
func (h *RestHandler) userSub(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path[len("/api/users/"):])
	if len(parts) < 1 {
		http.NotFound(w, r)
		return
	}
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	userID, err := strconv.Atoi(parts[0])
	if parts[0] == "me" {
		userID, err = pr.UserID, nil
		if !pr.HasUser() {
			err = errorf(`"me" needs a signed-in AMP user`)
		}
	}
	if err != nil {
		jsonErr(w, errorf("invalid user id"), 400)
		return
	}
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	switch {
	case sub == "role" && r.Method == http.MethodPatch:
		var body struct {
			Role  string `json:"role"`
			Grant bool   `json:"grant"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		if err := h.svc.SetUserRole(r.Context(), pr, userID, body.Role, body.Grant); err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"user_id": userID, "role": body.Role, "grant": body.Grant})

	case sub == "" && r.Method == http.MethodDelete:
		if err := h.svc.DeleteUser(r.Context(), pr, userID); err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"deleted": userID})

	case (sub == "deactivate" || sub == "reactivate") && r.Method == http.MethodPost:
		u, err := h.svc.SetUserDisabled(r.Context(), pr, userID, sub == "deactivate")
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"user": u})

	case sub == "tokens" && r.Method == http.MethodGet:
		tokens, err := h.svc.ListTokens(r.Context(), pr, userID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"tokens": tokens})

	case sub == "tokens" && r.Method == http.MethodPost:
		var body struct {
			Name          string `json:"name"`
			ExpiresInDays int    `json:"expires_in_days"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		tok, err := h.svc.CreateToken(r.Context(), pr, userID, body.Name, body.ExpiresInDays)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"token": tok})

	default:
		http.NotFound(w, r)
	}
}

// /api/tokens/:id  DELETE revokes a token.
func (h *RestHandler) tokenSub(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(strings.Trim(r.URL.Path[len("/api/tokens/"):], "/"))
	if err != nil || r.Method != http.MethodDelete {
		http.NotFound(w, r)
		return
	}
	if err := h.svc.RevokeToken(r.Context(), pr, id); err != nil {
		fail(w, err, 500)
		return
	}
	jsonOK(w, map[string]interface{}{"token_id": id, "revoked": true})
}

// /api/agents  GET lists your agent accounts (global admins: all);
// POST { name, team_ids } creates one you manage.
func (h *RestHandler) agents(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		agents, err := h.svc.ListAgents(r.Context(), pr)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"agents": agents})
	case http.MethodPost:
		var body struct {
			Name    string `json:"name"`
			TeamIDs []int  `json:"team_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		u, err := h.svc.CreateAgent(r.Context(), pr, body.Name, body.TeamIDs)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"agent": u})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// /api/audit?org_id=&project_id=&actor_id=&action=&before=&limit=
// Global admins read everything; org owners/admins their org; project
// owners their project.
func (h *RestHandler) audit(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	num := func(k string) int {
		n, _ := strconv.Atoi(q.Get(k))
		return n
	}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	entries, err := h.svc.Audit(r.Context(), pr, domain.AuditFilter{
		OrgID: num("org_id"), ProjectID: num("project_id"), ActorUserID: num("actor_id"),
		Action: q.Get("action"), BeforeID: before, Limit: num("limit"),
	})
	if err != nil {
		fail(w, err, 500)
		return
	}
	jsonOK(w, map[string]interface{}{"entries": entries})
}

// ---- /api/projects ----

func (h *RestHandler) projects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pr, ok := h.principal(w, r)
		if !ok {
			return
		}
		visible, err := h.svc.Authz.VisibleProjects(r.Context(), pr)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		projects, err := h.repo.ListProjects(r.Context())
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		projects = filterProjects(projects, visible)
		// Check if include_archived query param is set
		includeArchived := r.URL.Query().Get("include_archived") == "true"
		if includeArchived {
			archived, err := h.repo.ListArchivedProjects(r.Context())
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"projects": projects, "archived": filterProjects(archived, visible)})
		} else {
			jsonOK(w, map[string]interface{}{"projects": projects})
		}
	case http.MethodPost:
		pr, ok := h.principal(w, r)
		if !ok {
			return
		}
		var req domain.CreateProjectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, err, 400)
			return
		}
		var err error
		if req.Metadata, err = req.Metadata.Clean(); err != nil {
			jsonErr(w, err, 400)
			return
		}
		home, err := h.svc.ResolveProjectHome(r.Context(), pr, req.OrgID, req.TeamID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		req.OrgID, req.TeamID, req.CreatedBy = home.OrgID, home.TeamID, home.CreatedBy
		p, err := h.repo.CreateProject(r.Context(), req)
		if err != nil {
			fail(w, err, 500)
			return
		}
		h.hub.Publish(domain.Event{Type: domain.EventProjectCreated, ProjectID: p.ID, Payload: p, At: time.Now()})
		h.svc.Record(r.Context(), pr, projectAudit("project.create", p.ID, "created project "+p.Name))
		jsonOK(w, p)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// /api/projects/:id  and  /api/projects/:id/epics  and  /api/projects/:id/tasks
func (h *RestHandler) projectSub(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path[len("/api/projects/"):])
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}

	// Handle /api/projects/import before trying to parse as numeric ID
	if parts[0] == "import" {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		// Decode the request body as ExportBundle
		var bundle domain.ExportBundle
		if err := json.NewDecoder(r.Body).Decode(&bundle); err != nil {
			jsonErr(w, err, 400)
			return
		}

		// Read optional name and code overrides from query params
		newCode := r.URL.Query().Get("code")
		newName := r.URL.Query().Get("name")
		home, err := h.svc.ResolveProjectHome(r.Context(), pr, optIntParam(r, "org_id"), optIntParam(r, "team_id"))
		if err != nil {
			fail(w, err, 500)
			return
		}

		// Call ImportBundle to create the project hierarchy
		newProject, err := h.repo.ImportBundle(r.Context(), bundle, newCode, newName, home)
		if err != nil {
			fail(w, err, 400)
			return
		}
		h.svc.Record(r.Context(), pr, projectAudit("project.import", newProject.ID, "imported project "+newProject.Name+" from an export"))

		// Spawn the new project's actor via registry.Get — this triggers loadAndSpawnEpics
		pid, err := h.registry.Get(newProject.ID)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		_ = pid // Actor is now spawned and loading from DB

		// Recreate the KB docs, keeping their annotations and dates.
		for _, doc := range bundle.KBDocs {
			if h.kb == nil {
				slog.Warn("knowledge base not configured; skipping the bundle's KB docs", "docs", len(bundle.KBDocs))
				break
			}
			if err := h.kb.ImportDoc(r.Context(), kb.Project(newProject.ID), doc); err != nil {
				// Log but continue — don't fail the entire import if one doc fails
				slog.Warn("failed to import KB doc", "path", doc.Path, "err", err)
			}
		}

		// Publish EventProjectCreated to SSE hub
		h.hub.Publish(domain.Event{Type: domain.EventProjectCreated, ProjectID: newProject.ID, Payload: newProject, At: time.Now()})

		// Return the new project with 201 status
		w.WriteHeader(http.StatusCreated)
		jsonOK(w, newProject)
		return
	}

	projectID, err := strconv.Atoi(parts[0])
	if err != nil {
		jsonErr(w, errorf("invalid project id"), 400)
		return
	}
	// Every project route needs at least Member; stricter routes check again.
	lvl, ok := h.requireProject(w, r, pr, projectID, authz.Member)
	if !ok {
		return
	}

	if len(parts) == 1 {
		if r.Method == http.MethodPatch {
			var body domain.ProjectSettings
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, errorf("invalid JSON body"), 400)
				return
			}
			p, err := h.svc.UpdateProject(r.Context(), pr, projectID, body)
			if err != nil {
				fail(w, err, 500)
				return
			}
			h.hub.Publish(domain.Event{Type: domain.EventProjectUpdated, ProjectID: p.ID, Payload: p, At: time.Now()})
			jsonOK(w, map[string]interface{}{"project": p})
			return
		}
		p, err := h.repo.GetProject(r.Context(), projectID)
		if err != nil {
			jsonErr(w, err, 404)
			return
		}
		jsonOK(w, map[string]interface{}{"project": p})
		return
	}

	switch parts[1] {
	case "day-reports":
		// GET ?date=YYYY-MM-DD&tz=…: everyone on the project and the report
		// they saved for the day, narrowed to this project (the standup sheet).
		day, rows, err := h.svc.ProjectDayReports(r.Context(), pr, projectID, r.URL.Query().Get("date"), r.URL.Query().Get("tz"))
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"day": day, "people": rows})
		return

	case "handover":
		// POST {from, to, task_ids?, include_in_progress?, reason?}: move someone's open tickets to someone else.
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		var body struct {
			From              string `json:"from"`
			To                string `json:"to"`
			TaskIDs           []int  `json:"task_ids"`
			IncludeInProgress bool   `json:"include_in_progress"`
			Reason            string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.From == "" || body.To == "" {
			jsonErr(w, errorf("from and to are required (user id or email)"), 400)
			return
		}
		res, err := h.svc.Handover(r.Context(), pr, projectID, body.From, body.To, body.TaskIDs, body.IncludeInProgress, body.Reason)
		if err != nil {
			fail(w, err, 500)
			return
		}
		h.hub.Publish(domain.Event{Type: domain.EventTaskUpdated, ProjectID: projectID, Payload: map[string]interface{}{"handover": true}, At: time.Now()})
		jsonOK(w, map[string]interface{}{"results": res})
		return

	case "unmerged":
		// GET: completed tickets whose pushed work isn't merged yet.
		runs, err := h.svc.UnmergedWork(r.Context(), pr, projectID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"waiting_to_merge": runs, "count": len(runs)})
		return

	case "artifacts":
		h.projectArtifacts(w, r, pr, projectID)
		return

	case "trash":
		// GET: tickets, epics and stories deleted in the last 30 days.
		// POST trash/restore {task_ids, epic_ids, story_ids}: bring them back.
		if len(parts) > 2 && parts[2] == "restore" {
			if r.Method != http.MethodPost {
				http.Error(w, "POST required", 405)
				return
			}
			var body struct {
				TaskIDs  []int `json:"task_ids"`
				EpicIDs  []int `json:"epic_ids"`
				StoryIDs []int `json:"story_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.TaskIDs)+len(body.EpicIDs)+len(body.StoryIDs) == 0 {
				jsonErr(w, errorf("task_ids, epic_ids or story_ids is required"), 400)
				return
			}
			out := map[string]interface{}{"results": []service.RestoreResult{}}
			if len(body.EpicIDs)+len(body.StoryIDs) > 0 {
				groups, err := h.svc.RestoreGroups(r.Context(), pr, projectID, body.EpicIDs, body.StoryIDs)
				if err != nil {
					fail(w, err, 500)
					return
				}
				out["groups"] = groups
			}
			if len(body.TaskIDs) > 0 {
				res, err := h.svc.RestoreTasks(r.Context(), pr, projectID, body.TaskIDs)
				if err != nil {
					fail(w, err, 500)
					return
				}
				out["results"] = res
			}
			h.hub.Publish(domain.Event{Type: domain.EventTaskUpdated, ProjectID: projectID, Payload: map[string]interface{}{"restored": body.TaskIDs}, At: time.Now()})
			jsonOK(w, out)
			return
		}
		trash, err := h.svc.DeletedTasks(r.Context(), pr, projectID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		groups, err := h.svc.DeletedGroups(r.Context(), pr, projectID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"deleted": trash, "count": len(trash), "groups": groups})
		return

	case "approve-plan":
		// POST: a person approves tickets in the project's plan (per ticket,
		// AMP #2198). Optional body {"task_ids": [...], "as_of": RFC 3339,
		// "on_behalf": bool}. No task_ids: the caller's own awaiting tickets
		// (everyone's with on_behalf). as_of: only tickets planned at or
		// before it. Others' tickets need on_behalf and lead access. See
		// service.ApproveRequest / ApproveResult and KB
		// architecture/plan-approval.md.
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		var body service.ApproveRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			jsonErr(w, fmt.Errorf("approve-plan body: want {\"task_ids\": [ticket ids], \"as_of\": RFC 3339 timestamp, \"on_behalf\": bool}, all optional (as_of must be an RFC 3339 timestamp): %w", err), 400)
			return
		}
		// ApproveTickets publishes project.updated itself.
		res, err := h.svc.ApproveTickets(r.Context(), pr, projectID, body)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, res)
		return

	case "report":
		// GET ?since=&until= (RFC 3339; default the last 7 days) &group_by=<metadata key>
		q := r.URL.Query()
		until, since := openUntil(), time.Now().Add(-7*24*time.Hour)
		if v := q.Get("until"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				jsonErr(w, fmt.Errorf("until: %w", err), 400)
				return
			}
			until = t
		}
		if v := q.Get("since"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				jsonErr(w, fmt.Errorf("since: %w", err), 400)
				return
			}
			since = t
		}
		report, err := h.svc.ProjectReport(r.Context(), pr, projectID, since, until, q.Get("group_by"))
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, report)
		return

	case "plan-check":
		report, err := h.svc.CheckPlan(r.Context(), pr, projectID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, report)
		return

	case "members":
		members, err := h.svc.ProjectMembers(r.Context(), projectID)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"members": members, "my_access": lvl.String()})
		return

	case "teams":
		h.projectTeams(w, r, pr, projectID, lvl, parts[2:])
		return

	case "org":
		// POST {org_id, owner_team_id?}: put the project in an org, or move it.
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		var body struct {
			OrgID       int `json:"org_id"`
			OwnerTeamID int `json:"owner_team_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OrgID <= 0 {
			jsonErr(w, errorf("org_id is required"), 400)
			return
		}
		proj, removed, err := h.svc.MoveProjectToOrg(r.Context(), pr, projectID, body.OrgID, body.OwnerTeamID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"project": proj, "unlinked": removed})
		return

	case "webhooks":
		h.webhookCollection(w, r, pr, nil, &projectID)
		return

	case "reset":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		if lvl < authz.Owner {
			fail(w, authz.Forbidden("resetting a project requires project owner access"), 403)
			return
		}
		// 1. Wipe postgres
		if err := h.repo.ResetProject(r.Context(), projectID); err != nil {
			jsonErr(w, err, 500)
			return
		}
		// 2. Clear actor snapshot
		pid, err := h.registry.Get(projectID)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		replyCh := make(chan actor.ReplySimple, 1)
		h.registry.System().Root.Send(pid, &actor.MsgReset{ReplyCh: replyCh})
		actor.Await(r.Context(), replyCh)
		h.svc.Record(r.Context(), pr, projectAudit("project.reset", projectID, "reset the project: deleted all its epics, stories, and tickets"))
		jsonOK(w, map[string]interface{}{"project_id": projectID, "reset": true})
		return

	case "kb":
		// Proxy to the KB handler with project_id baked in as a query param.
		// Re-route to kbDoc with the sub-action from the URL.
		// /api/projects/:id/kb → list
		// /api/projects/:id/kb/doc → get/post/delete doc
		// /api/projects/:id/kb/search, tags, config
		kbAction := "list"
		if len(parts) >= 3 {
			kbAction = parts[2]
		}
		// Rewrite URL path and delegate.
		r2 := r.Clone(r.Context())
		q := r2.URL.Query()
		q.Set("project_id", strconv.Itoa(projectID))
		r2.URL.RawQuery = q.Encode()
		r2.URL.Path = "/api/kb/" + kbAction
		h.kbDoc(w, r2)
		return

	case "activity":
		since := r.URL.Query().Get("since")
		until := r.URL.Query().Get("until")
		activity, err := h.repo.GetProjectActivity(r.Context(), projectID, since, until)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"activity": activity, "count": len(activity)})
		return

	case "export":
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", 405)
			return
		}
		// Build the export bundle with project/epics/stories/tasks
		bundle, err := h.repo.BuildExportBundle(r.Context(), projectID)
		if err != nil {
			jsonErr(w, err, 404)
			return
		}

		// Every live KB doc, with annotations and original dates (none when
		// the knowledge base isn't configured). A failed read fails the export
		// rather than leave docs out of it.
		kbDocs := []domain.ExportKBDoc{}
		if h.kb != nil {
			if kbDocs, err = h.kb.ExportDocs(r.Context(), kb.Project(projectID)); err != nil {
				jsonErr(w, err, 500)
				return
			}
		}
		bundle.KBDocs = kbDocs

		// Set Content-Disposition header for file download
		filename := fmt.Sprintf("amp-export-%s-%s.json", bundle.Project.Code, time.Now().Format("2006-01-02"))
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
		jsonOK(w, bundle)
		return

	case "archive":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		if lvl < authz.Owner {
			fail(w, authz.Forbidden("archiving a project requires project owner access"), 403)
			return
		}
		p, err := h.repo.ArchiveProject(r.Context(), projectID)
		if err != nil {
			jsonErr(w, err, 404)
			return
		}
		h.hub.Publish(domain.Event{Type: domain.EventProjectArchived, ProjectID: p.ID, Payload: p, At: time.Now()})
		h.svc.Record(r.Context(), pr, projectAudit("project.archive", p.ID, "archived project "+p.Name))
		jsonOK(w, p)
		return

	case "restore":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		if lvl < authz.Owner {
			fail(w, authz.Forbidden("restoring a project requires project owner access"), 403)
			return
		}
		p, err := h.repo.RestoreProject(r.Context(), projectID)
		if err != nil {
			jsonErr(w, err, 404)
			return
		}
		h.hub.Publish(domain.Event{Type: domain.EventProjectRestored, ProjectID: p.ID, Payload: p, At: time.Now()})
		h.svc.Record(r.Context(), pr, projectAudit("project.restore", p.ID, "restored project "+p.Name))
		jsonOK(w, p)
		return

	case "epics":
		switch r.Method {
		case http.MethodGet:
			epics, err := h.repo.ListEpics(r.Context(), projectID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"epics": epics})
		case http.MethodPost:
			var req domain.CreateEpicRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				jsonErr(w, err, 400)
				return
			}
			var err error
			if req.Metadata, err = req.Metadata.Clean(); err != nil {
				jsonErr(w, err, 400)
				return
			}
			req.ProjectID = projectID
			req.CreatedBy = pr.UserRef()
			// Route through ProjectActor so EpicActor is spawned.
			pid, err := h.registry.Get(projectID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			replyCh := make(chan actor.ReplyCreateEpic, 1)
			h.registry.System().Root.Send(pid, &actor.MsgCreateEpic{Req: req, ReplyCh: replyCh})
			reply := actor.Await(r.Context(), replyCh)
			if reply.Err != nil {
				jsonErr(w, reply.Err, 500)
				return
			}
			jsonOK(w, reply.Epic)
		default:
			http.Error(w, "method not allowed", 405)
		}

	case "tasks":
		switch r.Method {
		case http.MethodGet:
			state := r.URL.Query().Get("state")
			view, err := h.svc.Authz.ParseTaskView(r.Context(), pr, r.URL.Query().Get("owner"))
			if err != nil {
				fail(w, err, 400)
				return
			}
			replyCh := make(chan actor.ReplyListTasks, 1)
			pid, err := h.registry.Get(projectID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			h.registry.System().Root.Send(pid, &actor.MsgListTasks{ProjectID: projectID, State: state, ReplyCh: replyCh})
			reply := actor.Await(r.Context(), replyCh)
			if reply.Err != nil {
				jsonErr(w, reply.Err, 500)
				return
			}
			tasks := authz.FilterTasks(reply.Tasks, view, pr.UserID)
			jsonOK(w, map[string]interface{}{"tasks": tasks, "count": len(tasks), "owner": view.Mode})

		case http.MethodPost:
			var body struct {
				domain.CreateTaskRequest
				// Owner: omitted = you, "none" = unassigned, or a project member.
				Owner string `json:"owner"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, err, 400)
				return
			}
			req := body.CreateTaskRequest
			if req.Metadata, err = req.Metadata.Clean(); err != nil {
				jsonErr(w, err, 400)
				return
			}
			req.ProjectID = projectID
			task, warnings, err := h.svc.CreateTask(r.Context(), pr, req, body.Owner)
			if err != nil {
				fail(w, err, 500)
				return
			}
			jsonOK(w, struct {
				*domain.Task
				Warnings []string `json:"warnings,omitempty"`
			}{task, warnings})

		default:
			http.Error(w, "method not allowed", 405)
		}

	default:
		http.NotFound(w, r)
	}
}

// ---- /api/epics/:id  and  /api/epics/:id/stories ----

func (h *RestHandler) epicSub(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path[len("/api/epics/"):])
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	epicID, err := strconv.Atoi(parts[0])
	if err != nil {
		jsonErr(w, errorf("invalid epic id"), 400)
		return
	}

	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	epic, err := h.repo.GetEpic(r.Context(), epicID)
	if err != nil {
		jsonErr(w, err, 404)
		return
	}
	if _, err := h.svc.Authz.RequireProject(r.Context(), pr, epic.ProjectID, authz.Member); err != nil {
		jsonErr(w, errorf(fmt.Sprintf("epic %d not found", epicID)), 404)
		return
	}
	// Changing or deleting the epic, or its owners, needs edit rights.
	if r.Method != http.MethodGet && !(len(parts) == 2 && parts[1] == "stories") {
		if _, err := h.svc.RequireEditEpic(r.Context(), pr, epicID); err != nil {
			fail(w, err, 403)
			return
		}
	}

	if len(parts) >= 2 && parts[1] == "owners" {
		h.groupingOwners(w, r, pr, "epic", epicID, epic.ProjectID, parts[2:])
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			jsonOK(w, map[string]interface{}{"epic": epic})

		case http.MethodPatch:
			var req domain.UpdateEpicRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				jsonErr(w, err, 400)
				return
			}
			if _, _, err := req.Metadata.Split(); err != nil {
				jsonErr(w, err, 400)
				return
			}
			req.EpicID = epicID
			if err := h.sendUpdate(epic.ProjectID, &actor.MsgUpdateEpic{Req: req, ReplyCh: make(chan actor.ReplySimple, 1)}); err != nil {
				jsonErr(w, err, 400)
				return
			}
			updated, err := h.repo.GetEpic(r.Context(), epicID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			h.hub.Publish(domain.Event{
				Type:      domain.EventEpicStateChanged,
				ProjectID: updated.ProjectID,
				Payload:   updated,
				At:        time.Now(),
			})
			jsonOK(w, map[string]interface{}{"epic": updated})

		case http.MethodDelete:
			if err := h.svc.DeleteEpic(r.Context(), pr, epic); err != nil {
				fail(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"epic_id": epicID, "deleted": true})

		default:
			http.Error(w, "method not allowed", 405)
		}
		return
	}

	if parts[1] == "stories" {
		switch r.Method {
		case http.MethodGet:
			stories, err := h.repo.ListStories(r.Context(), epicID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"stories": stories})
		case http.MethodPost:
			var req domain.CreateStoryRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				jsonErr(w, err, 400)
				return
			}
			var err error
			if req.Metadata, err = req.Metadata.Clean(); err != nil {
				jsonErr(w, err, 400)
				return
			}
			req.EpicID = epicID
			req.ProjectID = epic.ProjectID
			req.CreatedBy = pr.UserRef()
			// Route through ProjectActor → EpicActor so StoryActor is spawned.
			pid, err := h.registry.Get(epic.ProjectID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			replyCh := make(chan actor.ReplyCreateStory, 1)
			h.registry.System().Root.Send(pid, &actor.MsgCreateStory{Req: req, ReplyCh: replyCh})
			reply := actor.Await(r.Context(), replyCh)
			if reply.Err != nil {
				jsonErr(w, reply.Err, 500)
				return
			}
			jsonOK(w, reply.Story)
		default:
			http.Error(w, "method not allowed", 405)
		}
		return
	}

	http.NotFound(w, r)
}

// ---- /api/stories/:id ----

func (h *RestHandler) storySub(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path[len("/api/stories/"):])
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	storyID, err := strconv.Atoi(parts[0])
	if err != nil {
		jsonErr(w, errorf("invalid story id"), 400)
		return
	}
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	story, err := h.repo.GetStory(r.Context(), storyID)
	if err != nil {
		jsonErr(w, err, 404)
		return
	}
	if _, err := h.svc.Authz.RequireProject(r.Context(), pr, story.ProjectID, authz.Member); err != nil {
		jsonErr(w, errorf(fmt.Sprintf("story %d not found", storyID)), 404)
		return
	}
	if r.Method != http.MethodGet {
		if _, err := h.svc.RequireEditStory(r.Context(), pr, storyID); err != nil {
			fail(w, err, 403)
			return
		}
	}
	if len(parts) >= 2 && parts[1] == "owners" {
		h.groupingOwners(w, r, pr, "story", storyID, story.ProjectID, parts[2:])
		return
	}
	if len(parts) == 2 && parts[1] == "move" && r.Method == http.MethodPost {
		// POST {epic_id}: put the story, with its tickets, in another epic.
		var body struct {
			EpicID int `json:"epic_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		moved, err := h.svc.MoveStory(r.Context(), pr, storyID, body.EpicID)
		if err != nil {
			fail(w, err, 400)
			return
		}
		jsonOK(w, map[string]interface{}{"story": moved})
		return
	}

	switch r.Method {
	case http.MethodGet:
		jsonOK(w, map[string]interface{}{"story": story})

	case http.MethodPatch:
		var req domain.UpdateStoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, err, 400)
			return
		}
		if _, _, err := req.Metadata.Split(); err != nil {
			jsonErr(w, err, 400)
			return
		}
		req.StoryID = storyID
		if err := h.sendUpdate(story.ProjectID, &actor.MsgUpdateStory{Req: req, ReplyCh: make(chan actor.ReplySimple, 1)}); err != nil {
			jsonErr(w, err, 400)
			return
		}
		updated, err := h.repo.GetStory(r.Context(), storyID)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		h.hub.Publish(domain.Event{Type: domain.EventStoryUpdated, ProjectID: updated.ProjectID, Payload: updated, At: time.Now()})
		jsonOK(w, map[string]interface{}{"story": updated})

	case http.MethodDelete:
		if err := h.svc.DeleteStory(r.Context(), pr, story); err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"story_id": storyID, "deleted": true})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// sendUpdate routes an epic or story edit through the project's actors, so
// their in-memory copies stay current, and waits for the reply.
func (h *RestHandler) sendUpdate(projectID int, msg interface{}) error {
	pid, err := h.registry.Get(projectID)
	if err != nil {
		return err
	}
	var reply chan actor.ReplySimple
	switch m := msg.(type) {
	case *actor.MsgUpdateEpic:
		reply = m.ReplyCh
	case *actor.MsgUpdateStory:
		reply = m.ReplyCh
	}
	h.registry.System().Root.Send(pid, msg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return actor.Await(ctx, reply).Err
}

// ---- /api/tasks/:id  and sub-routes ----

func (h *RestHandler) taskSub(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path[len("/api/tasks/"):])
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	taskID, err := strconv.Atoi(parts[0])
	if err != nil {
		jsonErr(w, errorf("invalid task id"), 400)
		return
	}

	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	task, _, err := h.svc.LoadTask(r.Context(), pr, taskID, taskActionFor(r.Method, sub))
	if err != nil {
		fail(w, err, 404)
		return
	}
	pid, err := h.registry.Get(task.ProjectID)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			replyCh := make(chan actor.ReplyGetTask, 1)
			h.registry.System().Root.Send(pid, &actor.MsgGetTask{TaskID: taskID, ReplyCh: replyCh})
			reply := actor.Await(r.Context(), replyCh)
			if reply.Err != nil {
				jsonErr(w, reply.Err, 404)
				return
			}
			jsonOK(w, map[string]interface{}{"task": reply.Task})

		case http.MethodPatch:
			var req domain.UpdateTaskRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				jsonErr(w, err, 400)
				return
			}
			if _, _, err := req.Metadata.Split(); err != nil {
				jsonErr(w, err, 400)
				return
			}
			// Off the web UI, a person's ticket isn't switched to agent work.
			if err := h.svc.UpdateTask(r.Context(), pr, task, req); err != nil {
				fail(w, err, 400)
				return
			}
			updated, err := h.repo.GetTask(r.Context(), taskID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"task": updated})

		case http.MethodDelete:
			if _, err := h.svc.DeleteTask(r.Context(), pr, task); err != nil {
				fail(w, err, 500)
				return
			}
			h.hub.Publish(domain.Event{
				Type:      domain.EventTaskUpdated,
				ProjectID: task.ProjectID,
				Payload:   map[string]interface{}{"task_id": taskID, "deleted": true},
				At:        time.Now(),
			})
			jsonOK(w, map[string]interface{}{"task_id": taskID, "deleted": true})

		default:
			http.Error(w, "method not allowed", 405)
		}
		return
	}

	switch parts[1] {
	case "runs":
		if r.Method == http.MethodPost {
			// Record a run (e.g. a person marking the work merged).
			var run domain.Run
			if err := json.NewDecoder(r.Body).Decode(&run); err != nil {
				jsonErr(w, err, 400)
				return
			}
			created, err := h.svc.RecordRun(r.Context(), pr, taskID, run)
			if err != nil {
				fail(w, err, 500)
				return
			}
			h.hub.Publish(domain.Event{Type: domain.EventTaskUpdated, ProjectID: created.ProjectID, Payload: map[string]interface{}{"task_id": taskID, "run": created.ID}, At: time.Now()})
			jsonOK(w, created)
			return
		}
		runs, err := h.repo.TaskRuns(r.Context(), taskID)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		var u domain.Usage
		for _, run := range runs {
			u.Add(run)
		}
		jsonOK(w, map[string]interface{}{"runs": runs, "usage": u})

	case "history":
		history, err := h.repo.GetTicketHistory(r.Context(), taskID)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		if history == nil {
			history = []domain.ActivityLog{}
		}
		jsonOK(w, map[string]interface{}{"history": history, "count": len(history)})

	case "dispatch":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		var body struct {
			AgentID string `json:"agent_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.AgentID == "" {
			body.AgentID = "amp-worker"
		}
		if _, err := h.svc.Dispatch(r.Context(), pr, taskID, body.AgentID); err != nil {
			fail(w, err, 400)
			return
		}
		jsonOK(w, map[string]interface{}{"task_id": taskID, "dispatched_to": body.AgentID})

	case "start":
		// POST: a person starts their own human ticket.
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		t, err := h.svc.StartHumanTask(r.Context(), pr, taskID)
		if err != nil {
			fail(w, err, 400)
			return
		}
		jsonOK(w, map[string]interface{}{"task": t})

	case "complete":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		if task.IsHuman() {
			// A person finishing their human ticket: {note, pr_url, hours_worked}, all optional.
			var done service.HumanDone
			if r.ContentLength != 0 {
				if err := json.NewDecoder(r.Body).Decode(&done); err != nil {
					jsonErr(w, err, 400)
					return
				}
			}
			t, run, err := h.svc.CompleteHumanTask(r.Context(), pr, taskID, done, false)
			if err != nil {
				fail(w, err, 400)
				return
			}
			jsonOK(w, map[string]interface{}{"task_id": taskID, "state": "completed", "task": t, "run": run})
			return
		}
		// A person in the web UI completes any ticket they can work; other
		// callers (API tokens, MCP-audience tokens) only dispatched work.
		if _, err := h.svc.CompleteTask(r.Context(), pr, taskID); err != nil {
			fail(w, err, 400)
			return
		}
		jsonOK(w, map[string]interface{}{"task_id": taskID, "state": "completed"})

	case "unblock":
		// POST {answer}: answer an agent's flag (a ticket blocked with a
		// reason) and put the ticket back to ready.
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		var body struct {
			Answer string `json:"answer"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		t, err := h.svc.AnswerQuestion(r.Context(), pr, taskID, body.Answer)
		if err != nil {
			fail(w, err, 400)
			return
		}
		jsonOK(w, map[string]interface{}{"task": t})

	case "comments":
		switch r.Method {
		case http.MethodGet:
			replyCh := make(chan actor.ReplyGetComments, 1)
			h.registry.System().Root.Send(pid, &actor.MsgGetComments{TaskID: taskID, ReplyCh: replyCh})
			reply := actor.Await(r.Context(), replyCh)
			if reply.Err != nil {
				jsonErr(w, reply.Err, 500)
				return
			}
			comments := reply.Comments
			if comments == nil {
				comments = []domain.Comment{}
			}
			jsonOK(w, map[string]interface{}{"comments": comments})

		case http.MethodPost:
			var req domain.AddCommentRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				jsonErr(w, err, 400)
				return
			}
			req.TaskID = taskID
			req.AuthorUserID = pr.UserRef()
			if req.Author == "" || req.Author == "user" {
				req.Author = pr.Email
			}
			req.Author = h.svc.CommentAuthor(r.Context(), pr, req.Author)
			replyCh := make(chan actor.ReplyAddComment, 1)
			h.registry.System().Root.Send(pid, &actor.MsgAddComment{Req: req, ReplyCh: replyCh})
			reply := actor.Await(r.Context(), replyCh)
			if reply.Err != nil {
				jsonErr(w, reply.Err, 500)
				return
			}
			jsonOK(w, reply.Comment)

		default:
			http.Error(w, "method not allowed", 405)
		}

	case "dependencies":
		// PUT {dependency_ids: [...]}: the ticket's complete new list.
		if r.Method != http.MethodPut {
			http.Error(w, "PUT required", 405)
			return
		}
		var body struct {
			DependencyIDs []int `json:"dependency_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		updated, warnings, err := h.svc.SetTaskDependencies(r.Context(), pr, taskID, body.DependencyIDs)
		if err != nil {
			fail(w, err, 400)
			return
		}
		jsonOK(w, map[string]interface{}{"task": updated, "warnings": warnings})

	case "move":
		// POST {story_id}: put the ticket in another story.
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		var body struct {
			StoryID int `json:"story_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		updated, err := h.svc.MoveTask(r.Context(), pr, taskID, body.StoryID)
		if err != nil {
			fail(w, err, 400)
			return
		}
		jsonOK(w, map[string]interface{}{"task": updated})

	case "assign":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		var body struct {
			User string `json:"user"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		updated, err := h.svc.AssignTask(r.Context(), pr, taskID, body.User)
		if err != nil {
			fail(w, err, 400)
			return
		}
		jsonOK(w, map[string]interface{}{"task": updated})

	case "start_at":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		var body struct {
			StartAt *time.Time `json:"start_at"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		replyCh := make(chan actor.ReplySimple, 1)
		h.registry.System().Root.Send(pid, &actor.MsgSetTaskStartAt{
			TaskID:  taskID,
			StartAt: body.StartAt,
			ReplyCh: replyCh,
		})
		reply := actor.Await(r.Context(), replyCh)
		if reply.Err != nil {
			jsonErr(w, reply.Err, 400)
			return
		}
		jsonOK(w, map[string]interface{}{"task_id": taskID, "start_at": body.StartAt})

	default:
		http.NotFound(w, r)
	}
}

// taskActionFor maps a task route to the ownership rule its outer check
// needs. Reading and commenting need only project access; assign and
// dispatch are checked in full by the service; everything else that changes
// the ticket is "work" on it.
func taskActionFor(method, sub string) authz.TaskAction {
	switch {
	case method == http.MethodGet, sub == "comments", sub == "history", sub == "runs", sub == "assign", sub == "dispatch":
		return authz.TaskRead
	case sub == "" && method == http.MethodDelete:
		return authz.TaskDelete
	case sub == "dependencies", sub == "move", sub == "" && method == http.MethodPatch:
		// Planning: the ticket's text, dependencies, or story.
		return authz.TaskPlan
	}
	return authz.TaskWork
}

func filterProjects(ps []domain.Project, visible map[int]bool) []domain.Project {
	if visible == nil {
		return ps
	}
	out := make([]domain.Project, 0, len(ps))
	for _, p := range ps {
		if visible[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

func optIntParam(r *http.Request, name string) *int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil {
		return &v
	}
	return nil
}

// ---- helpers ----

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func errorf(msg string) error {
	return &simpleError{msg}
}

type simpleError struct{ msg string }

func (e *simpleError) Error() string { return e.msg }

func splitPath(p string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			if i > start {
				parts = append(parts, p[start:i])
			}
			start = i + 1
		}
	}
	if start < len(p) {
		parts = append(parts, p[start:])
	}
	return parts
}

// suppress unused import warning
var _ = context.Background

// ---- KB routes: /api/kb/* ----
// Also handles /api/projects/:id/kb/* (routed from projectSub)
//
// Every route names one knowledge base with exactly one of project_id,
// team_id, or org_id (see docs/kb-scopes-design.md). KB below means that.
//
// GET  /api/kb/info?KB                    → the KB's name and what you may do in it
// GET  /api/kb/sources?project_id=N       → the KBs around a project (its own, its org's, its teams')
// GET  /api/kb/config?project_id=N        → {typesense_url, api_key, collection} (global admins)
// GET  /api/kb/doc?KB&path=P              → full doc
// POST /api/kb/doc?KB                     → write doc (body: {path,title,content,tags,author,reason})
// DELETE /api/kb/doc?KB&path=P            → delete doc
// GET  /api/kb/list?KB&tag=T&limit=L&offset=O → list doc summaries (all unless limit is given)
// GET  /api/kb/search?KB&q=Q              → search results
// GET  /api/kb/tags?KB                    → tag counts
// POST /api/kb/annotate?KB                → annotate a doc (body: {path,text})
// POST /api/kb/resolve?KB                 → resolve or reopen an annotation (body: {path,index,resolved})
func (h *RestHandler) kbDoc(w http.ResponseWriter, r *http.Request) {
	// Parse sub-path: /api/kb/{action}
	sub := r.URL.Path[len("/api/kb/"):]
	action := sub
	if idx := len(action); idx == 0 {
		action = "doc"
	}

	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	ref := service.KBRef{Scope: q.Get("scope")}
	for _, f := range []struct {
		name string
		dst  *int
	}{{"project_id", &ref.ProjectID}, {"team_id", &ref.TeamID}, {"org_id", &ref.OrgID}} {
		if v := q.Get(f.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				jsonErr(w, errorf("invalid "+f.name), 400)
				return
			}
			*f.dst = n
		}
	}
	k, err := h.svc.ResolveKB(r.Context(), pr, ref)
	if err != nil {
		fail(w, err, 500)
		return
	}
	if (action == "doc" && (r.Method == http.MethodPost || r.Method == http.MethodDelete)) || action == "resolve" {
		if err := k.RequireWrite(); err != nil {
			fail(w, err, 403)
			return
		}
	}
	if k.Kind == kb.ScopeGlobal && r.Method != http.MethodGet {
		// The AMP guide changes only through the repo, and an annotation
		// on it would be visible to every org.
		fail(w, k.RequireWrite(), 403)
		return
	}
	if action == "info" {
		jsonOK(w, k)
		return
	}
	if action == "config" {
		// Typesense's admin key, valid for every KB: global admins only.
		// (The UI doesn't use this endpoint; it searches through /api/kb/search.)
		if !pr.Super {
			fail(w, authz.Forbidden("KB connection details are restricted to global admins"), 403)
			return
		}
		if k.Kind != kb.ScopeProject {
			jsonErr(w, errorf("config is only available for project KBs"), 400)
			return
		}
	}
	if h.kb == nil {
		jsonErr(w, errorf("the knowledge base is not configured"), 503)
		return
	}
	sc := k.Scope

	switch action {
	case "sources":
		if ref.ProjectID == 0 {
			jsonErr(w, errorf("project_id is required"), 400)
			return
		}
		sources, err := h.svc.KBSources(r.Context(), pr, ref.ProjectID, func(sc kb.Scope) int {
			n, _ := h.kb.CountDocs(r.Context(), sc)
			return n
		})
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"sources": sources})

	case "config":
		// Typesense connection info so a client can search directly
		// (checked above: global admins, project KBs).
		collectionName := fmt.Sprintf("kb_%d", sc.ID)
		jsonOK(w, map[string]interface{}{
			"typesense_url": h.kb.TypesenseURL(),
			"api_key":       h.kb.SearchAPIKey(),
			"collection":    collectionName,
		})

	case "doc":
		switch r.Method {
		case http.MethodGet:
			path := r.URL.Query().Get("path")
			if path == "" {
				jsonErr(w, errorf("path query param required"), 400)
				return
			}
			doc, err := h.kb.GetDoc(r.Context(), sc, path)
			if err != nil {
				jsonErr(w, err, 404)
				return
			}
			jsonOK(w, doc)

		case http.MethodPost:
			var body struct {
				Path    string   `json:"path"`
				Title   string   `json:"title"`
				Content string   `json:"content"`
				Tags    []string `json:"tags"`
				Author  string   `json:"author"`
				Reason  string   `json:"reason"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, err, 400)
				return
			}
			if body.Author == "" || body.Author == "user" {
				body.Author = pr.Email
			}
			if sc.Kind == kb.ScopeProject {
				body.Reason = ""
			}
			if designSystemDoc(body.Path) {
				jsonErr(w, errDesignSystemDoc, 409)
				return
			}
			doc, err := h.kb.WriteDoc(r.Context(), sc, body.Path, body.Title, body.Content, body.Author, body.Tags, strings.TrimSpace(body.Reason), nil)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, doc)

		case http.MethodDelete:
			path := r.URL.Query().Get("path")
			if path == "" {
				jsonErr(w, errorf("path query param required"), 400)
				return
			}
			if designSystemDoc(path) {
				jsonErr(w, errDesignSystemDoc, 409)
				return
			}
			if err := h.kb.DeleteDoc(r.Context(), sc, path, pr.Email); err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"deleted": true, "path": path})

		default:
			http.Error(w, "method not allowed", 405)
		}

	case "list":
		// The UI's KB page builds its tree from every doc, so with no limit
		// REST lists the whole KB; limit and offset page it like amp_kb_list.
		tag := r.URL.Query().Get("tag")
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		docs, total, err := h.kb.ListDocsPage(r.Context(), sc, tag, limit, offset)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		out := map[string]interface{}{"docs": docs, "count": len(docs), "total": total}
		if next := max(offset, 0) + len(docs); len(docs) > 0 && next < total {
			out["next_offset"] = next
		}
		jsonOK(w, out)

	case "search":
		if q.Get("q") == "" {
			jsonErr(w, errorf("q query param required"), 400)
			return
		}
		results, err := h.kb.Search(r.Context(), sc, q.Get("q"), nil, 10, 0, 0)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"results": results, "count": len(results)})

	case "tags":
		tags, err := h.kb.ListTags(r.Context(), sc)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"tags": tags})

	case "resolve":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Path     string `json:"path"`
			Index    *int   `json:"index"`
			Resolved *bool  `json:"resolved"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" || body.Index == nil {
			jsonErr(w, errorf("path and index are required"), 400)
			return
		}
		resolved := body.Resolved == nil || *body.Resolved
		doc, err := h.kb.ResolveAnnotation(r.Context(), sc, body.Path, *body.Index, resolved, pr.Email)
		if err != nil {
			status := 400 // index out of range
			if strings.Contains(err.Error(), "doc not found") {
				status = 404
			}
			jsonErr(w, err, status)
			return
		}
		jsonOK(w, doc)

	case "annotate":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var body struct {
			Path   string `json:"path"`
			Text   string `json:"text"`
			Author string `json:"author"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		if body.Path == "" || body.Text == "" {
			jsonErr(w, errorf("path and text are required"), 400)
			return
		}
		if body.Author == "" || body.Author == "user" {
			body.Author = pr.Email
		}
		ann, err := h.kb.AnnotateDoc(r.Context(), sc, body.Path, body.Text, body.Author)
		if err != nil {
			jsonErr(w, err, 404)
			return
		}
		jsonOK(w, ann)

	default:
		http.NotFound(w, r)
	}
}

// openUntil is the end of a range that runs "to now". Rows are stamped by
// the database's clock, which can be a little ahead of this server's, so
// the open end allows a minute: otherwise work recorded a moment ago can
// fall outside "now".
func openUntil() time.Time { return time.Now().Add(time.Minute) }

// traceMerged: POST /api/trace/merged {commit|branch|pr, merge_commit?} —
// CI reports a merge; every ticket whose runs produced that work is marked merged.
func (h *RestHandler) traceMerged(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var body struct {
		Commit      string `json:"commit"`
		Branch      string `json:"branch"`
		PR          string `json:"pr"`
		MergeCommit string `json:"merge_commit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, err, 400)
		return
	}
	ids, err := h.svc.MarkMerged(r.Context(), pr, body.Commit, body.Branch, body.PR, body.MergeCommit)
	if err != nil {
		fail(w, err, 500)
		return
	}
	jsonOK(w, map[string]interface{}{"merged_task_ids": ids, "count": len(ids)})
}

// trace: GET /api/trace?commit=…|branch=…|pr=… — the tickets whose recorded
// runs produced that code, in projects the caller can see.
func (h *RestHandler) trace(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	hits, err := h.svc.Trace(r.Context(), pr, q.Get("commit"), q.Get("branch"), q.Get("pr"))
	if err != nil {
		fail(w, err, 500)
		return
	}
	jsonOK(w, map[string]interface{}{"tickets": hits, "count": len(hits)})
}

// Design system docs are only written by publishing a version, so they always
// come from phases a person approved.
var errDesignSystemDoc = errorf("design-system/ docs are written only by publishing a design system version (its project's publish step); start a new version to change them")

func designSystemDoc(path string) bool {
	return strings.HasPrefix(strings.TrimPrefix(path, "/"), domain.DesignSystemPrefix)
}
