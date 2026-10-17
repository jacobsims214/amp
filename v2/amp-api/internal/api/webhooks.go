package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/service"
)

// Webhooks (rules in internal/service/webhooks.go):
//
//	GET  /api/projects/:id/webhooks          project owners
//	POST /api/projects/:id/webhooks          {name, url, format, events}  → includes the secret, once
//	GET  /api/orgs/:id/webhooks              org owners/admins
//	POST /api/orgs/:id/webhooks
//	PATCH  /api/webhooks/:id                 {name?, url?, format?, events?, active?}
//	DELETE /api/webhooks/:id
//	POST /api/webhooks/:id/rotate-secret     → the new secret
//	POST /api/webhooks/:id/test              queues a ping
//	GET  /api/webhooks/:id/deliveries?limit=

func (h *RestHandler) webhookCollection(w http.ResponseWriter, r *http.Request, pr authz.Principal, orgID, projectID *int) {
	switch r.Method {
	case http.MethodGet:
		hooks, err := h.svc.ListWebhooks(r.Context(), pr, orgID, projectID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"webhooks": hooks})
	case http.MethodPost:
		var c service.WebhookChange
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			jsonErr(w, err, 400)
			return
		}
		hook, err := h.svc.CreateWebhook(r.Context(), pr, orgID, projectID, c)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"webhook": hook})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (h *RestHandler) webhookSub(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path[len("/api/webhooks/"):])
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.Atoi(parts[0])
	if err != nil {
		jsonErr(w, errorf("invalid webhook id"), 400)
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
	ctx := r.Context()
	switch {
	case sub == "" && r.Method == http.MethodPatch:
		var c service.WebhookChange
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			jsonErr(w, err, 400)
			return
		}
		hook, err := h.svc.UpdateWebhook(ctx, pr, id, c)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"webhook": hook})
	case sub == "" && r.Method == http.MethodDelete:
		if err := h.svc.DeleteWebhook(ctx, pr, id); err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"webhook_id": id, "deleted": true})
	case sub == "rotate-secret" && r.Method == http.MethodPost:
		hook, err := h.svc.RotateWebhookSecret(ctx, pr, id)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"webhook": hook})
	case sub == "test" && r.Method == http.MethodPost:
		deliveryID, err := h.svc.TestWebhook(ctx, pr, id)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"delivery_id": deliveryID, "queued": true})
	case sub == "deliveries" && r.Method == http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		deliveries, err := h.svc.ListDeliveries(ctx, pr, id, limit)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, map[string]interface{}{"deliveries": deliveries})
	default:
		http.NotFound(w, r)
	}
}
