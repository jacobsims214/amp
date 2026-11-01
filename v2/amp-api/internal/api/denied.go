package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/service"
)

// Every REST route that answers 403 records access.denied (rate-limited
// per actor and route; KB decisions/audit-log-v2.md, #2428), unless the
// service already recorded a specific refusal for the request
// (plan.approve.denied, task.dispatch.denied, ...).

// deniedMux registers handlers wrapped by RestHandler.denied.
type deniedMux struct {
	mux *http.ServeMux
	h   *RestHandler
}

func (m *deniedMux) HandleFunc(pattern string, fn http.HandlerFunc) {
	m.mux.HandleFunc(pattern, m.h.denied(fn))
}

// statusRecorder notes the status a handler wrote and the start of its
// body (the refusal message).
type statusRecorder struct {
	http.ResponseWriter
	status int
	body   []byte
}

const maxDeniedBody = 2048

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.status == http.StatusForbidden && len(r.body) < maxDeniedBody {
		r.body = append(r.body, b[:min(len(b), maxDeniedBody-len(r.body))]...)
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// denied wraps fn so a 403 it writes is audited as access.denied.
func (h *RestHandler) denied(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		fn(rec, r)
		if rec.status != http.StatusForbidden {
			return
		}
		p, err := authz.FromContext(r.Context())
		if err != nil {
			return // not authenticated: the auth middleware answers those
		}
		h.svc.AuditAccessDenied(r.Context(), p, "route", r.Method+" "+r.URL.Path, accessScopeOf(r), errors.New(deniedReason(rec.body)))
	}
}

// accessScopeOf reads what a request targets from its path
// (/api/projects/{id}/..., /api/orgs/{id}, /api/teams/{id}, /api/tasks/{id},
// /api/epics/{id}, /api/stories/{id}) and its project_id/org_id query
// parameters, so its access.denied row is filed under that org or project.
func accessScopeOf(r *http.Request) service.AccessScope {
	var sc service.AccessScope
	ids := map[string]*int{"projects": &sc.ProjectID, "orgs": &sc.OrgID, "teams": &sc.TeamID,
		"tasks": &sc.TaskID, "epics": &sc.EpicID, "stories": &sc.StoryID}
	if parts := splitPath(strings.TrimPrefix(r.URL.Path, "/api/")); len(parts) >= 2 {
		if dst, ok := ids[parts[0]]; ok {
			if n, err := strconv.Atoi(parts[1]); err == nil && n > 0 {
				*dst = n
			}
		}
	}
	q := r.URL.Query()
	for k, dst := range map[string]*int{"project_id": &sc.ProjectID, "org_id": &sc.OrgID} {
		if *dst == 0 {
			if n, err := strconv.Atoi(q.Get(k)); err == nil && n > 0 {
				*dst = n
			}
		}
	}
	return sc
}

// deniedReason pulls the message out of a {"error": "..."} body, or
// keeps the body as is when it isn't one (or was cut short).
func deniedReason(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		return s
	}
	return "forbidden"
}
