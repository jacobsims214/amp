package api

import (
	"encoding/json"
	"net/http"

	"github.com/simstech/amp-api/internal/authz"
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
