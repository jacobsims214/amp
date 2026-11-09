package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/receipts"
	"github.com/simstech/amp-api/internal/service"
)

// Verification receipts (#126). Rules live in internal/service/receipts.go.
//
//	GET  /api/tasks/:id/receipt          {receipt, checks, evidence, status, held, attempts, settings}, or {receipt: null, settings}
//	GET  /api/tasks/:id/receipts         {receipts: [{receipt, checks, evidence, status, held}]}, newest first
//	POST /api/tasks/:id/receipt/accept   {note?}   a person in the web UI who can work the ticket
//	POST /api/tasks/:id/receipt/reject   {note}    the same; the note is required
//	GET  /api/projects/:id/receipts/summary        {mode, min_level, ..., tasks: [{task_id, status, held, has_media}], would_hold_last_7_days}
//	GET  /api/orgs/:id/receipts                    {mode}   org admin
//	PUT  /api/orgs/:id/receipts {mode}             org admin, web UI only
//	PUT  /api/receipt-uploads/<token>              evidence bytes; no session, the one-time token is the credential
//	GET  /api/receipts/<key>                       evidence (Range works); anyone who can read the ticket

// taskReceipt serves /api/tasks/:id/receipt[/accept|/reject] and
// /api/tasks/:id/receipts. parts is the path after /api/tasks/.
func (h *RestHandler) taskReceipt(w http.ResponseWriter, r *http.Request, p authz.Principal, taskID int, parts []string) {
	ctx := r.Context()
	if parts[1] == "receipts" {
		if len(parts) != 2 || r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		list, err := h.svc.ListTaskReceipts(ctx, p, taskID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]any{"receipts": list})
		return
	}
	if len(parts) == 2 {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		tr, err := h.svc.GetTaskReceipt(ctx, p, taskID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		if tr.ReceiptView == nil {
			jsonOK(w, map[string]any{"receipt": nil, "mode": tr.Settings.Mode, "settings": tr.Settings})
			return
		}
		jsonOK(w, tr)
		return
	}
	if len(parts) != 3 || (parts[2] != "accept" && parts[2] != "reject") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			jsonErr(w, errorf("invalid JSON body"), 400)
			return
		}
	}
	accept := h.svc.AcceptReceipt
	if parts[2] == "reject" {
		accept = h.svc.RejectReceipt
	}
	v, err := accept(ctx, p, taskID, body.Note)
	if err != nil {
		fail(w, err, 400)
		return
	}
	jsonOK(w, v)
}

// projectReceiptSummary serves GET /api/projects/:id/receipts/summary.
func (h *RestHandler) projectReceiptSummary(w http.ResponseWriter, r *http.Request, p authz.Principal, projectID int, parts []string) {
	if len(parts) != 3 || parts[2] != "summary" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	sum, err := h.svc.ProjectReceiptSummary(r.Context(), p, projectID)
	if err != nil {
		fail(w, err, 500)
		return
	}
	jsonOK(w, sum)
}

// orgReceipts serves /api/orgs/:id/receipts: the org's default mode.
func (h *RestHandler) orgReceipts(w http.ResponseWriter, r *http.Request, p authz.Principal, orgID int) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		if _, err := h.svc.Authz.RequireOrgAdmin(ctx, p, orgID); err != nil {
			fail(w, err, 403)
			return
		}
		org, err := h.repo.GetOrg(ctx, orgID)
		if err != nil {
			fail(w, err, 404)
			return
		}
		jsonOK(w, map[string]any{"mode": org.ReceiptsMode})
	case http.MethodPut:
		var body struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
			jsonErr(w, errorf("invalid JSON body"), 400)
			return
		}
		org, err := h.svc.SetOrgReceiptsMode(ctx, p, orgID, body.Mode)
		if err != nil {
			fail(w, err, 400)
			return
		}
		jsonOK(w, map[string]any{"mode": org.ReceiptsMode, "org": org})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// Evidence routes (#126, Phase 3).
const (
	// ReceiptUploadPrefix is the upload route; main leaves it out of the
	// auth middleware, like GitHubHookPrefix.
	ReceiptUploadPrefix = service.UploadPath
	// ReceiptEvidencePrefix serves stored evidence by key.
	ReceiptEvidencePrefix = "/api/receipts/"
)

// SetReceiptStore sets the receipt evidence store (nil: none configured).
func (h *RestHandler) SetReceiptStore(e *service.EvidenceStore) { h.svc.Evidence = e }

// receiptUpload serves PUT /api/receipt-uploads/<token>.
func (h *RestHandler) receiptUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		http.Error(w, "PUT required", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, ReceiptUploadPrefix)
	// The instance cap, whatever the store's own limit (the declared size,
	// at most this, is enforced by Put).
	body := http.MaxBytesReader(w, r.Body, h.svc.MaxEvidenceBytes()+1)
	ev, err := h.svc.StoreUpload(r.Context(), token, body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			err = &authz.Error{Status: 422, Msg: "the upload is longer than allowed; nothing was stored"}
		}
		fail(w, err, 500)
		return
	}
	jsonOK(w, map[string]any{"key": ev.Key, "name": ev.Name, "bytes": ev.Bytes, "sha256": ev.SHA256, "status": ev.Status})
}

// receiptEvidence serves GET /api/receipts/<key>: the evidence file, under
// the same safety headers as an artifact's content (writeArtifactContent):
// the content type from the allowlisted name, nosniff, the sandbox CSP, no
// referrer, same-origin CORP, and inline for media and text but attachment
// for trace.zip and har.json. HTML and SVG are never on the allowlist.
func (h *RestHandler) receiptEvidence(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, ReceiptEvidencePrefix)
	se, err := h.svc.EvidenceForServe(r.Context(), p, key)
	if err != nil {
		fail(w, err, 500)
		return
	}
	ct, ok := receipts.ContentType(se.Evidence.Name)
	if !ok {
		// A name that is no longer allowed is never served.
		fail(w, authz.NotFound("evidence not found"), 404)
		return
	}
	disp := "inline"
	if receipts.Attachment(se.Evidence.Name) {
		disp = "attachment"
	}
	hd := w.Header()
	hd.Set("Content-Type", ct)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", artifactSandbox)
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")
	hd.Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disp, se.Evidence.Name))
	hd.Set("ETag", `"`+se.Evidence.SHA256+`"`)
	hd.Set("Cache-Control", "private, no-cache")
	if err := se.Store.Serve(w, r, se.Evidence.Key); err != nil {
		for _, k := range []string{"Content-Disposition", "ETag", "Cache-Control"} {
			hd.Del(k)
		}
		if errors.Is(err, receipts.ErrNotFound) {
			slog.Warn("receipt evidence is stored in Postgres but missing from the store", "key", se.Evidence.Key)
			fail(w, authz.NotFound("evidence file is missing from the store"), 404)
			return
		}
		fail(w, err, 500)
	}
}
