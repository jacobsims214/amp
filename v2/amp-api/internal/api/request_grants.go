package api

// Team requester access administration over REST (epic 202, story 549, AMP
// #3975; approved product contract artifact 36 v2 §2 and §5, build slice B2's
// REST half and B1's invite half).
//
//	GET    /api/teams/:id/request-grants             who may submit to this inbox (in force first, then revoked)
//	POST   /api/teams/:id/request-grants             give one person submit access   {user}
//	DELETE /api/teams/:id/request-grants/:user       take it away
//	GET    /api/teams/:id/request-invites            the inbox's pending requester invitations
//	POST   /api/teams/:id/request-invites            invite a person as a requester  {email}
//	DELETE /api/teams/:id/request-invites/:invite    revoke a pending requester invitation
//
// Every one of them is a thin adapter over administration that already existed
// and was already reviewed: Service.GrantSubmitAccess, RevokeSubmitAccess and
// ListSubmitGrants (internal/service/request_grants.go, #3769) and
// Service.Invite / RevokeInvite / ListRequesterInvites (internal/service/orgs.go,
// #3767 and this ticket's team-scoped listing). It decodes a body or a path,
// maps the transport's own facts onto the service's input and encodes the
// service's answer. It decides nothing: who may administer a team, the tenant,
// validation, idempotency and every audit row are the service's, so a refusal
// is the same refusal the service gives every other caller.
//
// Registering through deniedMux (see RestHandler.register) is what puts the two
// shared boundaries in front of all six routes: submitScopeGate refuses a
// submit-only service credential before any handler runs, and denied() records
// a 403 as access.denied like every other refusal on this surface. A 404 is not
// recorded: the team surface answers 404 for a team the caller has no
// relationship with (loadTeam's rule, so a cross-tenant caller learns nothing),
// and "not found" is not an access event (decisions/audit-log-v2.md).
//
// # Submit access is not membership, and nothing here can make it so
//
// This is the whole reason the surface is team-addressed and separate from
// /api/orgs/:id/invites and /api/teams/:id/members:
//
//   - a grant gives a person the ability to send requests to one inbox and
//     touches no membership table (repository.GrantTeamSubmit writes only
//     team_submit_grants), so the granted customer still reaches zero projects
//     and RequireProject still answers 404;
//   - a requester invitation is bound to this team and kind=requester, and
//     accepting it provisions a customer identity and records acceptance, with
//     no org_members and no team_members row (AcceptInvitesList);
//   - the request bodies here have no field that could ask for a role, a
//     different team or an org, and one is refused by name rather than dropped
//     (see the two forbidden-field maps below). A caller cannot turn either
//     route into a membership vector by asking politely.
//
// # Deliberately no second way to mint a credential
//
// A submit-scoped service credential is minted through the existing, reviewed
// route — POST /api/users/:id/tokens with {"scope": "submit", "scope_team_id":
// <team>} (rest.go's userSub; service.CreateScopedToken, #3772) — which checks
// that the *issuer* may submit to that inbox and returns the secret exactly
// once. There is no mint route in this file, on purpose: a second mint would be
// a second definition of who may hold a narrowed credential, and the team
// administration screen needs no more than the existing one. What this ticket
// proves about it is the property that matters here: the secret appears once in
// the mint's answer, never in any listing, never in an audit row, and a
// submit-only credential cannot administer the inbox it may submit to (the
// scope gate refuses all six routes above with token.scope_denied).
//
// # No email is sent, and none could be
//
// An invitation is a row. Nothing on this path opens a connection to a mail
// server or an SMTP relay — the API imports no mailer at all (the only
// `net/mail` use in the module is address parsing in service/approval.go) — so
// inviting someone as a requester stores the intent and provisions the identity
// at their next sign-in. That is also why the tests run with no mail
// configuration and send nothing.

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Bodies and answers ----

// requestGrantBody is what a grant may send: the person, named the way the
// service resolves users ("me", a numeric id, or an email).
//
// There is deliberately no role, team, org or project field: submit access is
// one permission on one team, the team comes from the path, and the org comes
// from the team's own row. A payload naming one of them is refused by name
// (requestGrantForbiddenFields) rather than dropped, because a dropped role
// would leave the caller believing they had granted more than they did.
type requestGrantBody struct {
	User string `json:"user"`
}

// requestInviteBody is what a requester invitation may send: the recipient.
//
// The kind is the route (`request-invites` is the requester invitation), the
// team is the path and the org is the team's, so none of them is a field. The
// roles are not fields either: a requester invitation grants no role at all.
type requestInviteBody struct {
	Email string `json:"email"`
}

// grantNoMembershipRule is what every refusal of a membership-shaped field on
// these routes says. It is one sentence because there is one rule: submit
// access and requester invitations are not membership, and the membership
// routes are elsewhere.
const grantNoMembershipRule = "submit access is a revocable grant on one team's inbox and never membership: it reaches no project, so " +
	"this route grants no role. Team membership is POST /api/teams/{id}/members, and org membership is POST /api/orgs/{id}/members"

// requesterInviteRule is the same rule for the invitation route.
const requesterInviteRule = "a requester invitation provisions a customer identity and grants no org or team role; accepting it creates no " +
	"membership at all. Team membership is POST /api/teams/{id}/members, and an org or team invitation is POST /api/orgs/{id}/invites"

// requestGrantForbiddenFields are the grant body's membership-shaped fields, and
// the sentence that explains why each is not one. They are answered off the
// decoder's unknown-field refusal so the caller gets the rule rather than
// "unknown field".
var requestGrantForbiddenFields = map[string]string{
	"role": grantNoMembershipRule, "org_role": grantNoMembershipRule, "team_role": grantNoMembershipRule,
	"team_id": "the team is the path's: a grant is administration of one inbox and cannot name another",
	"org_id":  "the org is the team's own row, read from the path's team: a payload cannot choose a tenant",
	"project_id": "submit access confers nothing on any project, so a grant cannot name one: the requester reaches zero projects by " +
		"design (contract §2)",
	"scope": "a submit grant is one permission, not a credential: a scoped service credential is minted through POST /api/users/{id}/tokens " +
		"with scope \"submit\", which returns its secret once",
}

// requestInviteForbiddenFields are the invitation body's membership-shaped
// fields. `kind` is in the map for a caller's benefit: the route already is the
// requester invitation, and a body that says kind=member is a caller who meant
// the other invitation route.
var requestInviteForbiddenFields = map[string]string{
	"kind": "this route is the requester invitation: the customer identity it provisions is the point. An org or team invitation is " +
		"POST /api/orgs/{id}/invites with kind \"member\"",
	"org_role": requesterInviteRule, "team_role": requesterInviteRule,
	"role":    requesterInviteRule,
	"team_id": "the team is the path's: a requester invitation is bound to one inbox and cannot name another",
	"org_id":  "the org is the team's own row, read from the path's team: a payload cannot choose a tenant",
}

// requestGrantAnswer is one grant as the administration view gets it: the
// stored row and whether it is in force. The row carries the recipient's id,
// the grantor's id and the two timestamps — never a credential, never a role.
//
// There is no `changed` flag, deliberately. Granting is idempotent (the
// repository's upsert keeps an active grant's original grantor and time), so a
// first grant and a replay answer with the same row, and the only place the
// difference is recorded is the audit entry (details.already_active). A flag
// here would need a second read in front of the write, which is the
// check-then-write race the repository was written to avoid.
type requestGrantAnswer struct {
	TeamID int                     `json:"team_id"`
	Grant  *domain.TeamSubmitGrant `json:"grant"`
	Active bool                    `json:"active"`
}

// requestGrantListAnswer is a team's grants: in force first, then revoked (the
// service's own order), each with the recipient's email, display name and
// account kind. Revoked grants are listed on purpose — to a manager, "who may
// submit" and "who was taken away" are the same question.
type requestGrantListAnswer struct {
	TeamID int                        `json:"team_id"`
	Grants []domain.TeamSubmitGrantee `json:"grants"`
}

// requestInviteAnswer is one pending requester invitation. Its kind is always
// "requester" and its roles are always member (the invitation grants neither),
// which is what makes the answer safe to show on a management screen.
type requestInviteAnswer struct {
	TeamID int            `json:"team_id"`
	Invite *domain.Invite `json:"invite"`
}

// requestInviteListAnswer is the inbox's pending requester invitations.
type requestInviteListAnswer struct {
	TeamID  int             `json:"team_id"`
	Invites []domain.Invite `json:"invites"`
}

// ---- Grants ----

// teamRequestGrants serves GET (the listing) and POST (one grant) on
// /api/teams/:id/request-grants.
func (h *RestHandler) teamRequestGrants(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		grants, err := h.svc.ListSubmitGrants(r.Context(), p, teamID)
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, requestGrantListAnswer{TeamID: teamID, Grants: grants})
	case http.MethodPost:
		body, ok := decodeRequestGrant(w, r)
		if !ok {
			return
		}
		grant, err := h.svc.GrantSubmitAccess(r.Context(), p, teamID, body.User)
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		if grant == nil {
			// Unreachable through the service, which returns the stored row on
			// every success. Failing closed keeps a future caller from
			// answering 200 for an act with nothing to show for itself.
			fail(w, errorf("the grant write answered no row"), http.StatusInternalServerError)
			return
		}
		jsonOK(w, requestGrantAnswer{TeamID: teamID, Grant: grant, Active: grant.Active()})
	default:
		// Unreachable: GET and POST are registered as separate method+path pairs
		// and a third method falls through to /api/teams/ (teamSub), which
		// answers 404 — the same answer every other request route on this
		// surface gives a wrong method, rather than a 405 that would tell a
		// prober which methods exist.
		http.NotFound(w, r)
	}
}

// teamRequestGrantRevoke serves DELETE /api/teams/:id/request-grants/:user: the
// revocation, attributed to the caller. The path's user is resolved by the
// service exactly as the grant body's is ("me", an id, or an email), so an
// administration screen can revoke a row it listed without a second lookup.
//
// A pair with no grant row at all is the service's not-found; a grant that is
// already revoked is the no-op it is (200, and the audit row says so). Both are
// the service's, not this adapter's.
func (h *RestHandler) teamRequestGrantRevoke(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	user := strings.TrimSpace(r.PathValue("user"))
	if user == "" {
		fail(w, authz.BadRequest("name the person whose submit access to revoke: a user id, an email, or \"me\""), http.StatusBadRequest)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	grant, err := h.svc.RevokeSubmitAccess(r.Context(), p, teamID, user)
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	if grant == nil {
		fail(w, errorf("the revocation answered no row"), http.StatusInternalServerError)
		return
	}
	jsonOK(w, requestGrantAnswer{TeamID: teamID, Grant: grant, Active: grant.Active()})
}

// ---- Requester invitations ----

// teamRequestInvites serves GET (the pending list) and POST (one invitation) on
// /api/teams/:id/request-invites.
func (h *RestHandler) teamRequestInvites(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		invites, err := h.svc.ListRequesterInvites(r.Context(), p, teamID)
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, requestInviteListAnswer{TeamID: teamID, Invites: invites})
	case http.MethodPost:
		body, ok := decodeRequestInvite(w, r)
		if !ok {
			return
		}
		res, err := h.svc.InviteRequesterForTeam(r.Context(), p, teamID, body.Email)
		if err != nil {
			fail(w, err, http.StatusInternalServerError)
			return
		}
		if res == nil || res.Invite == nil {
			fail(w, errorf("the invitation answered no row"), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		jsonOK(w, requestInviteAnswer{TeamID: teamID, Invite: res.Invite})
	default:
		// Unreachable, for the reason the grant route's default gives: a third
		// method reaches teamSub's 404 before it reaches this handler.
		http.NotFound(w, r)
	}
}

// teamRequestInviteRevoke serves DELETE /api/teams/:id/request-invites/:invite.
//
// The team in the path is not decoration: the service refuses a revocation of an
// invitation that belongs to another team or to another org, so a manager of one
// inbox cannot revoke another inbox's invitation by changing the path.
func (h *RestHandler) teamRequestInviteRevoke(w http.ResponseWriter, r *http.Request) {
	teamID, ok := requestPathTeam(w, r)
	if !ok {
		return
	}
	inviteID, err := strconv.Atoi(strings.TrimSpace(r.PathValue("invite")))
	if err != nil || inviteID <= 0 {
		fail(w, authz.BadRequest("the invitation id in the path must be a whole number"), http.StatusBadRequest)
		return
	}
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if err := h.svc.RevokeRequesterInvite(r.Context(), p, teamID, inviteID); err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"team_id": teamID, "invite_id": inviteID, "revoked": true})
}

// ---- Decoding ----

// decodeRequestGrant reads one grant body: bounded, strict about unknown fields
// and one JSON value (decodeRequestMutation), with a membership-shaped field
// answered by name and the rule instead of by the decoder's "unknown field".
//
// `user` is required here rather than left to the service's ResolveUser, because
// an empty user is a caller mistake with a better answer than "user is
// required" arriving after the manager gate: it names the field.
func decodeRequestGrant(w http.ResponseWriter, r *http.Request) (requestGrantBody, bool) {
	var body requestGrantBody
	if err := decodeRequestMutation(w, r, &body, "submit-access grant"); err != nil {
		if field := unknownField(err); field != "" {
			if why, forbidden := requestGrantForbiddenFields[field]; forbidden {
				fail(w, authz.RequestInvalid("%q is not a field of a submit-access grant: %s", field, why), http.StatusBadRequest)
				return body, false
			}
		}
		fail(w, err, http.StatusBadRequest)
		return body, false
	}
	body.User = strings.TrimSpace(body.User)
	if body.User == "" {
		fail(w, authz.RequestInvalid(`a submit-access grant needs "user": a user id, an email, or "me"`), http.StatusBadRequest)
		return body, false
	}
	return body, true
}

// decodeRequestInvite reads one invitation body, with the same bound, strictness
// and by-name refusals as the grant body.
//
// The email shape is checked here because the service's requester path stores
// what it is given (inviteRequester) while its membership path validates — and an
// invitation for "bob" is a row that can never provision anyone. The rule is the
// membership path's own (`strings.Contains(email, "@")`), so the two invitation
// routes cannot disagree about what an email is.
func decodeRequestInvite(w http.ResponseWriter, r *http.Request) (requestInviteBody, bool) {
	var body requestInviteBody
	if err := decodeRequestMutation(w, r, &body, "requester invitation"); err != nil {
		if field := unknownField(err); field != "" {
			if why, forbidden := requestInviteForbiddenFields[field]; forbidden {
				fail(w, authz.RequestInvalid("%q is not a field of a requester invitation: %s", field, why), http.StatusBadRequest)
				return body, false
			}
		}
		fail(w, err, http.StatusBadRequest)
		return body, false
	}
	body.Email = strings.TrimSpace(body.Email)
	if !strings.Contains(body.Email, "@") {
		fail(w, authz.BadRequest("a valid email is required"), http.StatusBadRequest)
		return body, false
	}
	return body, true
}
