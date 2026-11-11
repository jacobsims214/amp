package api

// The REST boundary for submit-only service credentials (AMP #3772; approved
// contract artifact 36 v2 §5, build slice B5).
//
// A submit-scoped API token may make exactly one call: `POST
// /api/teams/{id}/requests`, the team-inbox submission, for the one team the
// token is bound to. Everything else is refused here, before the handler runs —
// every route registered through deniedMux (which is every /api route except
// the SSE stream, the GitHub hook and the receipt-upload prefix) plus the SSE
// stream itself, which is registered unwrapped because a 403 after it starts
// streaming is useless.
//
// This is a boundary, not the whole enforcement. The capability layer narrows
// the same credential independently (authz/token_scope.go: no project level, no
// org or team role, no inbox read, an empty visible-project set, and TeamSubmit
// only for the bound team), so a route added in future without this wrapper
// still reaches nothing rather than inheriting the account's authority. Both
// layers are deliberate: this one decides *what the credential may do*, that one
// decides *what it may see*, and neither can be undone by forgetting the other.
//
// The route is allow-listed by shape rather than by an exemption list, so a new
// route is refused by default. Submission's own authorization still applies
// after the gate: the handler's service call runs Checker.TeamSubmit, which for
// a scoped credential passes only for the bound team and only while the account
// could submit there anyway.

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
)

// isSubmitScopeRoute reports whether method+path is the one operation a
// submit-scoped credential may perform: a POST to /api/teams/{id}/requests,
// with no trailing sub-path (a further path segment is a different route — a
// request read, a comment, a claim — and none of those are the submission).
// It returns the team id in the path, so the caller can check the binding.
//
// The shape is checked here and the binding here too, in the gate: the
// submission's handler is a later slice, so "the bound team only" cannot be left
// to a handler that does not exist yet. A route that names no team — or names a
// team other than the bound one — is not the credential's submission.
func submitScopeRoute(method, path string) (teamID int, ok bool) {
	if method != http.MethodPost {
		return 0, false
	}
	rest, ok := strings.CutPrefix(path, "/api/teams/")
	if !ok {
		return 0, false
	}
	team, tail, ok := strings.Cut(rest, "/")
	if !ok || team == "" || tail != "requests" {
		return 0, false
	}
	id, err := strconv.Atoi(team)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// submitScopeGate refuses a submit-scoped credential on every route but its one
// submission. It is applied by deniedMux.HandleFunc, so registration is what
// enforces it: a handler added to the REST surface cannot forget the gate,
// because there is no other way to register one.
func (h *RestHandler) submitScopeGate(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := authz.FromContext(r.Context())
		if err != nil {
			// Not authenticated: the auth middleware answers those, and a
			// caller with no principal reaches no handler that needs one.
			fn(w, r)
			return
		}
		if p.SubmitOnly() {
			// Exactly one route, and only for the bound team. Anything else —
			// another route, or the submission route naming another team — is
			// refused here, before the handler, so the answer does not depend
			// on a handler existing yet.
			teamID, ok := submitScopeRoute(r.Method, r.URL.Path)
			if !ok || !p.AllowsSubmitScope(teamID) {
				fail(w, authz.TokenScopeForbidden(), http.StatusForbidden)
				return
			}
		}
		fn(w, r)
	}
}
