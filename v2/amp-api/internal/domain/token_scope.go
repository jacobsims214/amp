package domain

import (
	"fmt"
	"slices"
	"strings"
)

// API token scopes: how much of the account a token carries (AMP #3772;
// approved contract artifact 36 v2 §5, build slice B5 of
// discoveries/team-inbox-approved-build-slices.md).
//
// A token is otherwise a full stand-in for the account that owns it: whatever
// the person can do, the token can do, everywhere. That is right for an agent
// working under someone's name and wrong for a service client whose only job
// is to send requests to one team's inbox — such a client should be able to
// submit and nothing else, so that a leaked connector credential costs one
// inbox's worth of junk rather than the project.
//
// There are deliberately only two scopes, and the vocabulary is closed (the
// api_tokens CHECK mirrors this list), for the same reason users.kind is: a
// value nobody recognises must be unstorable rather than silently meaning
// "full".
//
//   - TokenScopeFull is every token that existed before scopes: unchanged
//     behaviour, the account's whole authority, as if the scope did not exist.
//   - TokenScopeSubmit is the submit-only credential: it may send a request to
//     exactly one team's inbox (ScopeTeamID) and may do nothing else — no
//     reading tickets, projects, requests or inboxes, no listing, no grant
//     administration, no minting tokens, no approving anything.
//
// The scope is an **attenuation** of the account's authority, never an
// expansion: a submit-only token on team T still answers every ordinary
// authorization question as its user would, minus everything outside T, and
// the team it names is only permissive if its issuer could submit there when
// the token was minted. Nothing at request time re-derives that (the row is
// the record of it), so a token's scope is fixed for the token's life:
// api_tokens has no update path for it, only revocation.

const (
	// TokenScopeFull: the token carries the account's whole authority (the
	// behaviour of every token minted before scopes existed).
	TokenScopeFull = "full"
	// TokenScopeSubmit: the token may only submit a request to one team's
	// inbox, named by its ScopeTeamID.
	TokenScopeSubmit = "submit"
)

// TokenScopes is the closed set of scopes, in the order the schema's CHECK
// accepts them.
var TokenScopes = []string{TokenScopeFull, TokenScopeSubmit}

// NormalizeTokenScope validates a requested scope (" Submit " is submit).
// Empty means full, so a caller that predates scopes — or omits the field —
// mints exactly what it used to. An unknown value is an error rather than a
// silent full-scope token, because the safe default for a credential is the
// narrower one and only an explicit request may widen it.
func NormalizeTokenScope(s string) (string, error) {
	scope := strings.ToLower(strings.TrimSpace(s))
	if scope == "" {
		return TokenScopeFull, nil
	}
	if !slices.Contains(TokenScopes, scope) {
		return "", fmt.Errorf("token scope must be one of %s", strings.Join(TokenScopes, ", "))
	}
	return scope, nil
}

// SubmitOnly reports whether the token may only submit to its team's inbox.
func (t APIToken) SubmitOnly() bool { return t.Scope == TokenScopeSubmit }
