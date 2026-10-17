package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
)

// ---- Daily reports, velocity, and metadata (rules in internal/service) ----
//
// GET  /api/day-reports?user=&from=&to=&tz=   saved reports (yours or someone's in your orgs)
// GET  /api/day-reports/draft?date=&tz=        your report for a day, built now (not saved)
// PUT  /api/day-reports/{YYYY-MM-DD}?tz=       save your report for a day (body: {summary})
// GET  /api/day-reports/people                 whose reports you can read
// GET  /api/velocity?user=&from=&to=&tz=       completed tickets per day
// POST /api/metadata/search                    {kind, match, project_id?, limit?}
// GET  /api/metadata/inherited?kind=&id=       what an item inherits from above it

func (h *RestHandler) dayReports(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/day-reports"), "/")
	switch {
	case sub == "" && r.Method == http.MethodGet:
		reports, err := h.svc.DayReports(r.Context(), pr, q.Get("user"), q.Get("from"), q.Get("to"), q.Get("tz"))
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"reports": reports, "count": len(reports)})
	case sub == "draft" && r.Method == http.MethodGet:
		rep, err := h.svc.DayReportDraft(r.Context(), pr, q.Get("date"), q.Get("tz"))
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"report": rep})
	case sub == "people" && r.Method == http.MethodGet:
		people, err := h.svc.ReportPeople(r.Context(), pr)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"people": people})
	case sub != "" && !strings.Contains(sub, "/") && r.Method == http.MethodPut:
		var body struct {
			Summary string `json:"summary"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, errorf("invalid JSON body"), 400)
			return
		}
		rep, err := h.svc.SaveDayReport(r.Context(), pr, sub, q.Get("tz"), body.Summary)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"report": rep})
	default:
		http.NotFound(w, r)
	}
}

func (h *RestHandler) velocity(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	days, err := h.svc.Velocity(r.Context(), pr, q.Get("user"), q.Get("from"), q.Get("to"), q.Get("tz"))
	if err != nil {
		fail(w, err, 500)
		return
	}
	jsonOK(w, map[string]interface{}{"days": days})
}

// myWork is the home page: my open tickets, flags in projects I can see,
// and my agents. GET /api/my-work
func (h *RestHandler) myWork(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	work, err := h.svc.MyWork(r.Context(), pr)
	if err != nil {
		fail(w, err, 500)
		return
	}
	jsonOK(w, work)
}

func (h *RestHandler) metadata(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.principal(w, r)
	if !ok {
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/api/metadata/") {
	case "search":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		var body struct {
			Kind      string                 `json:"kind"`
			Match     map[string]interface{} `json:"match"`
			ProjectID int                    `json:"project_id"`
			Limit     int                    `json:"limit"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, errorf("invalid JSON body"), 400)
			return
		}
		found, err := h.svc.FindByMetadata(r.Context(), pr, body.Kind, body.Match, body.ProjectID, body.Limit)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"results": found, "count": len(found)})
	case "inherited":
		kind := r.URL.Query().Get("kind")
		id, err := strconv.Atoi(r.URL.Query().Get("id"))
		if err != nil {
			jsonErr(w, errorf("id is required"), 400)
			return
		}
		projectID, err := h.projectOfItem(r, kind, id)
		if err != nil {
			fail(w, err, 404)
			return
		}
		if _, ok := h.requireProject(w, r, pr, projectID, authz.Member); !ok {
			return
		}
		inherited, err := h.repo.InheritedMetadata(r.Context(), kind, id)
		if err != nil {
			fail(w, err, 404)
			return
		}
		jsonOK(w, map[string]interface{}{"inherited": inherited})
	default:
		http.NotFound(w, r)
	}
}

// projectOfItem returns the project a project, epic, story, or task is in.
func (h *RestHandler) projectOfItem(r *http.Request, kind string, id int) (int, error) {
	ctx := r.Context()
	switch kind {
	case "project":
		return id, nil
	case "epic":
		if e, err := h.repo.GetEpic(ctx, id); err == nil {
			return e.ProjectID, nil
		}
	case "story":
		if s, err := h.repo.GetStory(ctx, id); err == nil {
			return s.ProjectID, nil
		}
	case "task":
		if t, err := h.repo.GetTask(ctx, id); err == nil {
			return t.ProjectID, nil
		}
	default:
		return 0, authz.BadRequest("kind must be project, epic, story, or task")
	}
	return 0, authz.NotFound("%s %d not found", kind, id)
}
