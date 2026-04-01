package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/repository"
)

// RestHandler serves the REST API consumed by the frontend.
//
// Routes:
//
//	GET  /api/projects
//	POST /api/projects
//	GET  /api/projects/:id
//	GET  /api/projects/:id/epics
//	GET  /api/projects/:id/tasks
//	POST /api/projects/:id/tasks
//	GET  /api/epics/:id
//	GET  /api/epics/:id/stories
//	GET  /api/stories/:id
//	GET  /api/tasks/:id
//	GET  /api/tasks/:id/comments
//	POST /api/tasks/:id/comments
//	GET  /api/tasks/:id/history
//	POST /api/tasks/:id/dispatch
//	POST /api/tasks/:id/complete
//	GET  /api/events              ← SSE stream
type RestHandler struct {
	registry *actor.Registry
	repo     *repository.Repo
	hub      *hub.Hub
	kb       *kb.Service
}

func NewRestHandler(registry *actor.Registry, repo *repository.Repo, h *hub.Hub, kbSvc *kb.Service) *RestHandler {
	return &RestHandler{registry: registry, repo: repo, hub: h, kb: kbSvc}
}

func (h *RestHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/projects", h.projects)
	mux.HandleFunc("/api/projects/", h.projectSub)
	mux.HandleFunc("/api/epics/", h.epicSub)
	mux.HandleFunc("/api/stories/", h.storySub)
	mux.HandleFunc("/api/tasks/", h.taskSub)
	mux.HandleFunc("/api/kb/", h.kbDoc)
	mux.Handle("/api/events", h.hub)
}

// ---- /api/projects ----

func (h *RestHandler) projects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		projects, err := h.repo.ListProjects(r.Context())
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"projects": projects})
	case http.MethodPost:
		var req domain.CreateProjectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, err, 400)
			return
		}
		p, err := h.repo.CreateProject(r.Context(), req)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		h.hub.Publish(domain.Event{Type: domain.EventProjectCreated, ProjectID: p.ID, Payload: p, At: time.Now()})
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
	projectID, err := strconv.Atoi(parts[0])
	if err != nil {
		jsonErr(w, errorf("invalid project id"), 400)
		return
	}

	if len(parts) == 1 {
		p, err := h.repo.GetProject(r.Context(), projectID)
		if err != nil {
			jsonErr(w, err, 404)
			return
		}
		jsonOK(w, map[string]interface{}{"project": p})
		return
	}

	switch parts[1] {
	case "reset":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
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
		<-replyCh
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
			req.ProjectID = projectID
			// Route through ProjectActor so EpicActor is spawned.
			pid, err := h.registry.Get(projectID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			replyCh := make(chan actor.ReplyCreateEpic, 1)
			h.registry.System().Root.Send(pid, &actor.MsgCreateEpic{Req: req, ReplyCh: replyCh})
			reply := <-replyCh
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
			replyCh := make(chan actor.ReplyListTasks, 1)
			pid, err := h.registry.Get(projectID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			h.registry.System().Root.Send(pid, &actor.MsgListTasks{ProjectID: projectID, State: state, ReplyCh: replyCh})
			reply := <-replyCh
			if reply.Err != nil {
				jsonErr(w, reply.Err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"tasks": reply.Tasks, "count": len(reply.Tasks)})

		case http.MethodPost:
			var req domain.CreateTaskRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				jsonErr(w, err, 400)
				return
			}
			req.ProjectID = projectID
			replyCh := make(chan actor.ReplyCreateTask, 1)
			pid, err := h.registry.Get(projectID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			h.registry.System().Root.Send(pid, &actor.MsgCreateTask{Req: req, ReplyCh: replyCh})
			reply := <-replyCh
			if reply.Err != nil {
				jsonErr(w, reply.Err, 500)
				return
			}
			jsonOK(w, reply.Task)

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

	if len(parts) == 1 {
		e, err := h.repo.GetEpic(r.Context(), epicID)
		if err != nil {
			jsonErr(w, err, 404)
			return
		}
		jsonOK(w, map[string]interface{}{"epic": e})
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
			epic, err := h.repo.GetEpic(r.Context(), epicID)
			if err != nil {
				jsonErr(w, err, 404)
				return
			}
			var req domain.CreateStoryRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				jsonErr(w, err, 400)
				return
			}
			req.EpicID = epicID
			req.ProjectID = epic.ProjectID
			// Route through ProjectActor → EpicActor so StoryActor is spawned.
			pid, err := h.registry.Get(epic.ProjectID)
			if err != nil {
				jsonErr(w, err, 500)
				return
			}
			replyCh := make(chan actor.ReplyCreateStory, 1)
			h.registry.System().Root.Send(pid, &actor.MsgCreateStory{Req: req, ReplyCh: replyCh})
			reply := <-replyCh
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

	s, err := h.repo.GetStory(r.Context(), storyID)
	if err != nil {
		jsonErr(w, err, 404)
		return
	}
	jsonOK(w, map[string]interface{}{"story": s})
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

	task, err := h.repo.GetTask(r.Context(), taskID)
	if err != nil {
		jsonErr(w, err, 404)
		return
	}
	pid, err := h.registry.Get(task.ProjectID)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}

	if len(parts) == 1 {
		replyCh := make(chan actor.ReplyGetTask, 1)
		h.registry.System().Root.Send(pid, &actor.MsgGetTask{TaskID: taskID, ReplyCh: replyCh})
		reply := <-replyCh
		if reply.Err != nil {
			jsonErr(w, reply.Err, 404)
			return
		}
		jsonOK(w, map[string]interface{}{"task": reply.Task})
		return
	}

	switch parts[1] {
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
		replyCh := make(chan actor.ReplySimple, 1)
		h.registry.System().Root.Send(pid, &actor.MsgDispatchTask{TaskID: taskID, AgentID: body.AgentID, ReplyCh: replyCh})
		reply := <-replyCh
		if reply.Err != nil {
			jsonErr(w, reply.Err, 400)
			return
		}
		jsonOK(w, map[string]interface{}{"task_id": taskID, "dispatched_to": body.AgentID})

	case "complete":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		replyCh := make(chan actor.ReplySimple, 1)
		h.registry.System().Root.Send(pid, &actor.MsgCompleteTask{TaskID: taskID, ReplyCh: replyCh})
		reply := <-replyCh
		if reply.Err != nil {
			jsonErr(w, reply.Err, 400)
			return
		}
		jsonOK(w, map[string]interface{}{"task_id": taskID, "state": "completed"})

	case "comments":
		switch r.Method {
		case http.MethodGet:
			replyCh := make(chan actor.ReplyGetComments, 1)
			h.registry.System().Root.Send(pid, &actor.MsgGetComments{TaskID: taskID, ReplyCh: replyCh})
			reply := <-replyCh
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
			replyCh := make(chan actor.ReplyAddComment, 1)
			h.registry.System().Root.Send(pid, &actor.MsgAddComment{Req: req, ReplyCh: replyCh})
			reply := <-replyCh
			if reply.Err != nil {
				jsonErr(w, reply.Err, 500)
				return
			}
			jsonOK(w, reply.Comment)

		default:
			http.Error(w, "method not allowed", 405)
		}

	default:
		http.NotFound(w, r)
	}
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
// GET  /api/kb/config?project_id=N        → {typesense_url, api_key, collection}
// GET  /api/kb/doc?project_id=N&path=P    → full doc
// POST /api/kb/doc?project_id=N           → write doc (body: {path,title,content,tags,author})
// DELETE /api/kb/doc?project_id=N&path=P  → delete doc
// GET  /api/kb/list?project_id=N&tag=T    → list doc summaries
// GET  /api/kb/search?project_id=N&q=Q   → search results
// GET  /api/kb/tags?project_id=N          → tag counts
func (h *RestHandler) kbDoc(w http.ResponseWriter, r *http.Request) {
	// Parse sub-path: /api/kb/{action}
	sub := r.URL.Path[len("/api/kb/"):]
	action := sub
	if idx := len(action); idx == 0 {
		action = "doc"
	}

	projectIDStr := r.URL.Query().Get("project_id")
	if projectIDStr == "" {
		jsonErr(w, errorf("project_id query param required"), 400)
		return
	}
	projectID, err := strconv.Atoi(projectIDStr)
	if err != nil {
		jsonErr(w, errorf("invalid project_id"), 400)
		return
	}

	switch action {
	case "config":
		// Returns Typesense connection info so the UI can search directly.
		collectionName := "kb_" + projectIDStr
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
			doc, err := h.kb.GetDoc(r.Context(), projectID, path)
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
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, err, 400)
				return
			}
			if body.Author == "" {
				body.Author = "user"
			}
			doc, err := h.kb.WriteDoc(r.Context(), projectID, body.Path, body.Title, body.Content, body.Author, body.Tags)
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
			if err := h.kb.DeleteDoc(r.Context(), projectID, path); err != nil {
				jsonErr(w, err, 500)
				return
			}
			jsonOK(w, map[string]interface{}{"deleted": true, "path": path})

		default:
			http.Error(w, "method not allowed", 405)
		}

	case "list":
		tag := r.URL.Query().Get("tag")
		docs, err := h.kb.ListDocs(r.Context(), projectID, tag)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"docs": docs, "count": len(docs)})

	case "search":
		q := r.URL.Query().Get("q")
		if q == "" {
			jsonErr(w, errorf("q query param required"), 400)
			return
		}
		results, err := h.kb.Search(r.Context(), projectID, q, nil, 10)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"results": results, "count": len(results)})

	case "tags":
		tags, err := h.kb.ListTags(r.Context(), projectID)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"tags": tags})

	default:
		http.NotFound(w, r)
	}
}
