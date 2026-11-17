package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// ArtifactUploadPrefix is the one-time artifact upload route (#4937); like
// ReceiptUploadPrefix, main leaves it out of the auth middleware.
const ArtifactUploadPrefix = service.ArtifactUploadPath

// artifactUpload serves PUT /api/artifact-uploads/<token>.
func (h *RestHandler) artifactUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		http.Error(w, "PUT required", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, ArtifactUploadPrefix)
	body := http.MaxBytesReader(w, r.Body, domain.MaxArtifactBytes+1)
	a, err := h.svc.StoreArtifactUpload(r.Context(), token, body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			err = &authz.Error{Status: 422, Msg: "the upload is longer than 10 MB; nothing was published"}
		}
		fail(w, err, 500)
		return
	}
	if h.hub != nil {
		h.hub.Publish(domain.Event{Type: domain.EventArtifactUpdated, ProjectID: a.ProjectID, Payload: a, At: time.Now()})
	}
	jsonOK(w, map[string]any{"artifact": a, "url": a.URL, "version": a.CurrentVersion})
}
