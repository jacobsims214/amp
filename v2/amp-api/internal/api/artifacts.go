package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// SetAppURL tells the handler AMP's public address, for links to artifacts.
func (h *RestHandler) SetAppURL(u string) { h.svc.AppURL = u }

// SetArtifactRetention sets how long unkept artifacts live after their last version.
func (h *RestHandler) SetArtifactRetention(d time.Duration) { h.svc.ArtifactRetention = d }

// RunArtifactPurge deletes expired artifacts every interval until ctx ends.
func (h *RestHandler) RunArtifactPurge(ctx context.Context, interval time.Duration) {
	h.svc.RunArtifactPurge(ctx, interval)
}

// artifactSandbox is the CSP every artifact's content is served with. The
// sandbox (without allow-same-origin) gives the document an opaque origin:
// its scripts run, but it can't read AMP's cookies or storage, or call AMP's
// API as the viewer — opened in the viewer's iframe or on its own.
const artifactSandbox = "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads"

// publishArtifactBody is a publish over REST: text in content, or binary in
// content_base64.
type publishArtifactBody struct {
	Title         string `json:"title"`
	Kind          string `json:"kind"`
	TaskID        *int   `json:"task_id"`
	Content       string `json:"content"`
	ContentBase64 string `json:"content_base64"`
	Note          string `json:"note"`
}

func (b publishArtifactBody) bytes() ([]byte, error) {
	if b.ContentBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(b.ContentBase64)
		if err != nil {
			return nil, authz.BadRequest("content_base64 isn't valid base64")
		}
		return raw, nil
	}
	return []byte(b.Content), nil
}

// projectArtifacts: GET /api/projects/:id/artifacts[?task_id=] lists them;
// POST publishes a new one.
func (h *RestHandler) projectArtifacts(w http.ResponseWriter, r *http.Request, p authz.Principal, projectID int) {
	switch r.Method {
	case http.MethodGet:
		list, err := h.svc.ListArtifacts(r.Context(), p, projectID, optIntParam(r, "task_id"))
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"artifacts": list, "count": len(list)})
	case http.MethodPost:
		var body publishArtifactBody
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, domain.MaxArtifactBytes*2)).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		content, err := body.bytes()
		if err != nil {
			fail(w, err, 400)
			return
		}
		a, err := h.svc.PublishArtifact(r.Context(), p, service.PublishArtifactRequest{ProjectID: projectID, TaskID: body.TaskID,
			Title: body.Title, Kind: body.Kind, Content: content, Note: body.Note})
		if err != nil {
			fail(w, err, 500)
			return
		}
		h.publishArtifactEvent(a)
		jsonOK(w, map[string]interface{}{"artifact": a})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// artifactSub serves /api/artifacts/:id and below:
//
//	GET  :id                 the artifact and its versions
//	GET  :id/content         the content (?version=N, default the latest), sandboxed
//	POST :id/versions        publish a new version {content | content_base64, note, title}
//	POST :id/review          a person's verdict {approve: bool, note, version}
//	POST :id/keep            {keep: bool}: keep it (never expires), or let it expire again
//	POST :id/let-expire      dismiss the expiry warning
//	GET  expiring?days=14    the caller's artifacts that expire soon
func (h *RestHandler) artifactSub(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path[len("/api/artifacts/"):])
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	if parts[0] == "expiring" && r.Method == http.MethodGet {
		p, ok := h.principal(w, r)
		if !ok {
			return
		}
		days, err := strconv.Atoi(r.URL.Query().Get("days"))
		if err != nil || days <= 0 {
			days = 14
		}
		list, err := h.svc.ExpiringArtifacts(r.Context(), p, time.Duration(days)*24*time.Hour)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"artifacts": list, "count": len(list)})
		return
	}
	id, err := strconv.Atoi(parts[0])
	if err != nil {
		jsonErr(w, errorf("invalid artifact id"), 400)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		a, versions, err := h.svc.GetArtifact(r.Context(), p, id)
		if err != nil {
			fail(w, err, 404)
			return
		}
		jsonOK(w, map[string]interface{}{"artifact": a, "versions": versions})

	case sub == "content" && r.Method == http.MethodGet:
		version, _ := strconv.Atoi(r.URL.Query().Get("version"))
		a, v, err := h.svc.ArtifactContent(r.Context(), p, id, version)
		if err != nil {
			fail(w, err, 404)
			return
		}
		writeArtifactContent(w, a, v, version > 0)

	case sub == "versions" && r.Method == http.MethodPost:
		var body publishArtifactBody
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, domain.MaxArtifactBytes*2)).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		content, err := body.bytes()
		if err != nil {
			fail(w, err, 400)
			return
		}
		a, err := h.svc.PublishArtifact(r.Context(), p, service.PublishArtifactRequest{ArtifactID: id, Title: body.Title,
			Kind: body.Kind, Content: content, Note: body.Note})
		if err != nil {
			fail(w, err, 500)
			return
		}
		h.publishArtifactEvent(a)
		jsonOK(w, map[string]interface{}{"artifact": a})

	case sub == "review" && r.Method == http.MethodPost:
		var body struct {
			Approve bool   `json:"approve"`
			Note    string `json:"note"`
			Version int    `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		a, err := h.svc.ReviewArtifact(r.Context(), p, id, body.Version, body.Approve, body.Note)
		if a != nil {
			h.publishArtifactEvent(a)
		}
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"artifact": a})

	case sub == "keep" && r.Method == http.MethodPost:
		var body struct {
			Keep bool `json:"keep"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, 400)
			return
		}
		a, err := h.svc.KeepArtifact(r.Context(), p, id, body.Keep)
		if err != nil {
			fail(w, err, 500)
			return
		}
		h.publishArtifactEvent(a)
		jsonOK(w, map[string]interface{}{"artifact": a})

	case sub == "let-expire" && r.Method == http.MethodPost:
		a, err := h.svc.LetArtifactExpire(r.Context(), p, id)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"artifact": a})

	default:
		http.Error(w, "not found", 404)
	}
}

// writeArtifactContent serves one version. Everything goes out under the
// sandbox CSP and nosniff; markdown and text as plain text (the UI renders
// markdown itself). A pinned version never changes, so it can be cached.
func writeArtifactContent(w http.ResponseWriter, a *domain.Artifact, v *domain.ArtifactVersion, pinned bool) {
	ct := v.ContentType
	if a.Kind == domain.ArtifactMarkdown {
		ct = "text/plain; charset=utf-8"
	}
	hd := w.Header()
	hd.Set("Content-Type", ct)
	// Browsers won't run their PDF viewer in a sandboxed document; a PDF's
	// own scripts run inside the viewer, not with AMP's origin.
	if a.Kind != domain.ArtifactPDF {
		hd.Set("Content-Security-Policy", artifactSandbox)
	}
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")
	hd.Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", artifactFilename(a, v)))
	hd.Set("ETag", `"`+v.SHA256+`"`)
	if pinned {
		hd.Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		hd.Set("Cache-Control", "private, no-cache")
	}
	_, _ = w.Write(v.Content)
}

func artifactFilename(a *domain.Artifact, v *domain.ArtifactVersion) string {
	ext := map[string]string{"html": "html", "svg": "svg", "markdown": "md", "pdf": "pdf", "text": "txt"}[a.Kind]
	if a.Kind == domain.ArtifactImage {
		ext = strings.TrimPrefix(strings.SplitN(v.ContentType, ";", 2)[0], "image/")
	}
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		}
		return '-'
	}, a.Title)
	return fmt.Sprintf("artifact-%d-v%d-%s.%s", a.ID, v.Version, strings.Trim(slug, "-"), ext)
}

func (h *RestHandler) publishArtifactEvent(a *domain.Artifact) {
	h.hub.Publish(domain.Event{Type: domain.EventArtifactUpdated, ProjectID: a.ProjectID, Payload: a, At: time.Now()})
}
