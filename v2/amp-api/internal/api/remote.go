package api

import (
	"encoding/json"
	"net/http"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// Remote control (epic 165): the org setting and the capability flag. The
// relay routes themselves are registered only when the instance switch is on
// (protocol.md section 8).

// SetRemoteControl sets the instance's remote control configuration: the
// instance switch (RELAY_ENABLED, Helm relay.enabled) and whether auth is on.
func (h *RestHandler) SetRemoteControl(cfg service.RemoteConfig) { h.svc.Remote = cfg }

// Service is the handler's service layer, for wiring that needs it (the relay).
func (h *RestHandler) Service() *service.Service { return h.svc }

// remoteCapability is /api/me's capabilities.remote_control for the caller.
func (h *RestHandler) remoteCapability(r *http.Request) (domain.RemoteCapability, error) {
	p, err := authz.FromContext(r.Context())
	if err != nil {
		// No AMP user behind the token: not a person remote control can serve.
		p = authz.Principal{}
	}
	return h.svc.RemoteCapability(r.Context(), p)
}

// orgRemoteControl serves /api/orgs/:id/remote-control.
//
//	GET  → {"enabled", "idle_timeout_min", "max_lifetime_hours", "device_pin_days"}   org owners/admins
//	PUT  {"enabled", "idle_timeout_min"?, "max_lifetime_hours"?, "device_pin_days"?}   org owners/admins, web UI only
//
// idle_timeout_min and max_lifetime_hours are 0 for "no limit", the default
// (#2424); an older client that doesn't send max_lifetime_hours keeps it.
//
// Members left out of a PUT keep their current values. Turning it off ends
// the org's live sessions.
func (h *RestHandler) orgRemoteControl(w http.ResponseWriter, r *http.Request, p authz.Principal, orgID int) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		pol, err := h.svc.GetOrgRemotePolicy(ctx, p, orgID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, pol)
	case http.MethodPut:
		var body struct {
			Enabled        *bool `json:"enabled"`
			IdleTimeoutMin   *int  `json:"idle_timeout_min"`
			MaxLifetimeHours *int  `json:"max_lifetime_hours"`
			DevicePinDays    *int  `json:"device_pin_days"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
			jsonErr(w, authz.Coded(400, domain.RemoteCodeMalformed, "bad JSON: %v", err), 400)
			return
		}
		cur, err := h.svc.GetOrgRemotePolicy(ctx, p, orgID)
		if err != nil {
			fail(w, err, 500)
			return
		}
		if body.Enabled != nil {
			cur.Enabled = *body.Enabled
		}
		if body.IdleTimeoutMin != nil {
			cur.IdleTimeoutMin = *body.IdleTimeoutMin
		}
		if body.MaxLifetimeHours != nil {
			cur.MaxLifetimeHours = *body.MaxLifetimeHours
		}
		if body.DevicePinDays != nil {
			cur.DevicePinDays = *body.DevicePinDays
		}
		pol, err := h.svc.SetOrgRemotePolicy(ctx, p, orgID, cur)
		if err != nil {
			fail(w, err, 500)
			return
		}
		jsonOK(w, pol)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
