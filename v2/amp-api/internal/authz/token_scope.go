package authz

// Submit-only service credentials: what a scoped API token may do (AMP #3772;
// approved contract artifact 36 v2 §5, build slice B5 of
// discoveries/team-inbox-approved-build-slices.md).
//
// A token is otherwise the account behind it: `Principal.Super`, the project
// `Level` ladder, org and team roles and submit grants all answer exactly as
// they would for the person or agent who minted it. That is right for an agent
// working under someone's name and wrong for a connector whose only job is to
// send requests to one team's inbox. So a token may be minted with
// domain.TokenScopeSubmit, which attenuates all of that to one operation:
// submitting a request to the one team the token is bound to.
//
// Two rules make this an attenuation and never an expansion, and both are
// enforced here rather than at each surface:
//
//   - **Exactly one operation, fail-closed.** AllowsSubmitScope is the whole
//     of what a scoped credential may reach: the submission itself, on its own
//     team. Every other route and every other tool is refused — reads, lists,
//     project and request internals, grants, token minting, approvals — whether
//     or not the account behind the token could have done it. The surfaces ask
//     this (api.RestHandler's gate, mcp.Server's tool middleware) before the
//     handler runs, so a route added later is refused by default instead of
//     inheriting the account's authority.
//
//   - **The account's own right still applies.** A scoped token on team T is
//     not a substitute for the account's standing there: TeamSubmit still
//     resolves the team membership, org role or submit grant from the rows on
//     the call. So revoking a person's grant, removing them from the team or
//     losing their org role takes their token's submit ability away on the very
//     next call, exactly as it takes the person's away (build slice B3's
//     per-call re-check). The scope can only narrow, never widen.
//
// The guards below are deliberately ordered *before* any `p.Super`
// short-circuit: a global admin who mints a submit-only token gets a token that
// can submit to one team, not one that can reach everything. Super stays on the
// principal because it is a fact about the account (and TeamSubmit's ordinary
// path uses it when the caller administers the inbox); the scope is what every
// read and every other capability answers first.

import (
	"context"

	"github.com/simstech/amp-api/internal/domain"
)

// CodeTokenScopeDenied is the stable code on every refusal of a submit-scoped
// credential used outside its one operation, so a client can tell "this
// credential may only submit" from "you are not allowed to do that" (the
// ticket surface's codes) and from the request surface's (request.*).
const CodeTokenScopeDenied = "token.scope_denied"

// SubmitOnly reports whether p is a submit-scoped service credential: a token
// that may reach nothing but the one submission it was minted for.
func (p Principal) SubmitOnly() bool { return p.Scope == domain.TokenScopeSubmit }

// AllowsSubmitScope is the whole of what a submit-scoped credential may do:
// submit to teamID, which must be the team it is bound to.
//
// It answers for the bound team only, and only about the submission — it does
// not itself say the account may submit there (TeamSubmit does that, from the
// rows). A full-scope principal never asks this: for one, Scope is
// TokenScopeFull (or empty, before scopes existed) and the ordinary
// authorization checks decide everything, unchanged.
func (p Principal) AllowsSubmitScope(teamID int) bool {
	return p.SubmitOnly() && teamID != 0 && teamID == p.ScopeTeamID
}

// TokenScopeForbidden is the refusal for a submit-scoped credential used
// outside its one operation. It names what the credential may do and names no
// ids, teams or resources: the credential is already scoped to one team, and a
// refusal that echoed what it was pointed at would answer questions the scope
// exists to keep unanswered.
func TokenScopeForbidden() error {
	return Coded(403, CodeTokenScopeDenied,
		"this API token may only submit requests to its own team's inbox; it can't read or change anything else. "+
			"Use a full-scope token for that.")
}

// requireNotScoped refuses a submit-scoped credential outright. Every
// capability below that a scoped token must never reach calls it first, before
// any Super short-circuit, so no ordering accident can let the account's
// authority through a narrowed credential.
func (p Principal) requireNotScoped() error {
	if p.SubmitOnly() {
		return TokenScopeForbidden()
	}
	return nil
}

// RequireUnscoped refuses a submit-scoped credential, naming what it tried to
// do ("mint API tokens", "list tokens"). Services call it at the entry point of
// every capability a narrowed credential must never reach — the same rule the
// Checker's guards make for project, org, team and inbox access, for the
// capabilities that have no Checker method of their own.
func (p Principal) RequireUnscoped(what string) error {
	if p.SubmitOnly() {
		return Coded(403, CodeTokenScopeDenied,
			"this API token may only submit requests to its own team's inbox, so it can't %s. Use a full-scope token for that.", what)
	}
	return nil
}

// ---- The guards the capability checks below call ----

// scopeProjectLevel is ProjectLevel's answer for a scoped credential: none, on
// every project, whatever the account's own level is.
func scopeProjectLevel(p Principal, projectID int) (Level, bool) {
	if !p.SubmitOnly() {
		return None, false
	}
	return None, true
}

// scopedOrgRole is OrgRole's answer for a scoped credential: no role in any
// org. It is "" rather than an error so callers that ask "what is my role"
// simply see no membership.
func scopedOrgRole(p Principal) (string, bool) {
	if !p.SubmitOnly() {
		return "", false
	}
	return "", true
}

// scopedInboxRole is InboxRole's answer for a scoped credential: no standing on
// any inbox, its own team included. A token that may submit to an inbox may not
// read it (contract §2: submitting is not seeing), so the team it is bound to is
// not readable through it either.
func scopedInboxRole(p Principal) (InboxRole, bool) {
	if !p.SubmitOnly() {
		return InboxNone, false
	}
	return InboxNone, true
}

// scopedVisibleProjects is VisibleProjects' answer for a scoped credential: the
// empty set, never nil. Nil means "every project" in that method's contract, so
// returning nil here would hand a submit-only token the whole instance through
// the SSE filter — the one place the difference is load-bearing.
func scopedVisibleProjects(p Principal) (map[int]bool, bool) {
	if !p.SubmitOnly() {
		return nil, false
	}
	return map[int]bool{}, true
}

// scopeTeamSubmit is TeamSubmit's answer for a scoped credential. It passes only
// when the team is the bound one *and* the account itself may still submit
// there, so the scope narrows the account's standing and follows it when it
// changes. Any other team is "not found": a scoped token learns nothing about
// teams it was not bound to.
func (c *Checker) scopeTeamSubmit(ctx context.Context, p Principal, teamID int) (bool, error) {
	if !p.SubmitOnly() {
		return false, nil
	}
	if !p.AllowsSubmitScope(teamID) {
		return true, NotFound("team %d not found", teamID)
	}
	// The same question an unscoped principal asks, on the account's own rows:
	// team membership, org owner/admin, or an in-force submit grant.
	unscoped := p
	unscoped.Scope, unscoped.ScopeTeamID = domain.TokenScopeFull, 0
	return true, c.teamSubmit(ctx, unscoped, teamID)
}
