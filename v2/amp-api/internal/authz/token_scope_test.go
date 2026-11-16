package authz

// Submit-only service credentials — the capability layer (AMP #3772, build
// slice B5; approved contract artifact 36 v2 §5).
//
// The claim this file proves is the one the credential's safety rests on: a
// submit-scoped principal is an *attenuation* of its account, so every ordinary
// capability answers as narrowed, not as the account would. Concretely:
//
//   - it reaches no project level, no visible project, no org role, no team role
//     and no inbox read — even when its account is a global admin. The visible
//     set is asserted non-nil specifically because nil means "every project" in
//     VisibleProjects' contract, and that one difference would hand a narrowed
//     credential the whole instance through the SSE filter
//     (TestSubmitScopeNarrowsEveryCapability);
//   - TeamSubmit passes for its bound team and only while the account could
//     submit there anyway, and learns nothing about any other team
//     (TestSubmitScopeTeamSubmitIsTheOnlyShape);
//   - it can never approve, mark a person's ticket done, or be "a person in the
//     web UI" — the acceptance path stays human (TestSubmitScopeIsNeverTheUI).
//
// The narrow-scope guards are pure except where the answer comes from the rows,
// so TestSubmitScopeToolAndRouteShapes and the pure assertions run anywhere;
// the Checker-level tests need Postgres and the real migrations, and skip
// without TEST_DATABASE_URL like the TeamSubmit and request-visibility tests:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/authz -run SubmitScope -count=1

import (
	"context"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
)

// scoped returns a submit-scoped version of p, bound to teamID. This is the
// principal the middleware builds from a submit-only token: Super and the
// account's ids are still there, because they are facts about the account —
// the scope is what every capability answers first.
func scoped(p Principal, teamID int) Principal {
	p.Scope, p.ScopeTeamID = domain.TokenScopeSubmit, teamID
	return p
}

// TestSubmitScopePredicates covers the two pure questions the surfaces ask.
func TestSubmitScopePredicates(t *testing.T) {
	full := Principal{UserID: 7, Scope: domain.TokenScopeFull}
	empty := Principal{UserID: 7} // a Dex session: no scope at all
	bound := scoped(Principal{UserID: 7}, 42)

	for _, p := range []Principal{full, empty} {
		if p.SubmitOnly() {
			t.Fatalf("principal %+v reads as submit-only; only TokenScopeSubmit may", p)
		}
		if p.AllowsSubmitScope(42) {
			t.Fatalf("principal %+v allowed a submit-scope operation", p)
		}
	}
	if !bound.SubmitOnly() {
		t.Fatal("a submit-scoped principal does not read as submit-only")
	}
	if !bound.AllowsSubmitScope(42) {
		t.Fatal("a submit-scoped principal may not perform its own submission")
	}
	// The team is the binding, not a hint: another team, zero (no team named)
	// and a negative id are all refusals.
	for _, teamID := range []int{0, -1, 41, 43} {
		if bound.AllowsSubmitScope(teamID) {
			t.Fatalf("a credential bound to team 42 allowed team %d", teamID)
		}
	}
}

// TestTokenScopeForbiddenShape pins the refusal: 403 with its own stable code, a
// message that says what the credential may do, and nothing that names the
// resource it was pointed at.
func TestTokenScopeForbiddenShape(t *testing.T) {
	err := TokenScopeForbidden()
	if got := StatusOf(err, 500); got != 403 {
		t.Fatalf("StatusOf = %d, want 403", got)
	}
	if got := CodeOf(err); got != CodeTokenScopeDenied {
		t.Fatalf("CodeOf = %q, want %q", got, CodeTokenScopeDenied)
	}
	msg := err.Error()
	for _, want := range []string{"submit", "full-scope"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q does not mention %q", msg, want)
		}
	}
	// No ids, no team names, no resource paths: the refusal is the same bytes
	// whatever the credential was pointed at.
	for _, not := range []string{"/api/", "amp_", "team ", "#", "42"} {
		if strings.Contains(msg, not) {
			t.Fatalf("refusal %q contains %q; a scope refusal names nothing", msg, not)
		}
	}

	// RequireUnscoped is the same refusal with the attempt named, and passes
	// an ordinary principal silently.
	if err := (Principal{}).RequireUnscoped("mint API tokens"); err != nil {
		t.Fatalf("an unscoped principal was refused: %v", err)
	}
	err = scoped(Principal{UserID: 7}, 42).RequireUnscoped("mint API tokens")
	if got := CodeOf(err); got != CodeTokenScopeDenied || StatusOf(err, 500) != 403 {
		t.Fatalf("RequireUnscoped = %v (%d/%q), want a 403 %s", err, StatusOf(err, 500), got, CodeTokenScopeDenied)
	}
	if !strings.Contains(err.Error(), "mint API tokens") {
		t.Fatalf("RequireUnscoped refusal does not name what it refused: %v", err)
	}
}

// TestSubmitScopeIsNeverTheUI is the acceptance boundary: a scoped credential
// is never a person in the web UI, so every person-only act — approving a plan
// or an artifact, marking a person's own ticket done, accepting an outcome — is
// refused to it. `Channel` is forged to the UI's on purpose: the token check is
// what must hold, not the channel label.
func TestSubmitScopeIsNeverTheUI(t *testing.T) {
	p := scoped(Principal{UserID: 7, Channel: auth.ChannelUI}, 42)
	if p.InUI() {
		t.Fatal("a token principal reads as a person in the web UI")
	}
	if err := RequireUI(p, "Requester outcome acceptance", "/request/REQ-1"); err == nil {
		t.Fatal("a submit-scoped credential may accept a requester outcome")
	}
}

// ---- Checker-level: the capability answers as narrowed, from the rows ----

// TestSubmitScopeNarrowsEveryCapability asks every Checker question a surface
// can ask, for a scoped credential bound to the inbox team, and asserts each
// one answers as narrowed — including for an account that is a global admin.
func TestSubmitScopeNarrowsEveryCapability(t *testing.T) {
	f := newSubmitFixture(t)
	ctx := f.ctx

	// The most privileged possible account: an org owner who is also a global
	// admin and a member of the inbox team. If the scope were advisory, this is
	// the principal that would show it.
	admin := Principal{UserID: f.admin, Super: true, Scope: domain.TokenScopeFull}
	if err := f.repo.SetTeamMember(ctx, f.team, f.admin, domain.TeamRoleMember); err != nil {
		t.Fatalf("admin team member: %v", err)
	}
	// Prove the baseline first, so a narrowing assertion cannot pass because
	// the account had nothing to narrow.
	if lvl, err := f.check.ProjectLevel(ctx, admin, f.project); err != nil || lvl < Super {
		t.Fatalf("baseline: admin project level = %v, %v; the fixture's account must reach Super", lvl, err)
	}

	narrowed := scoped(Principal{UserID: f.admin, Super: true}, f.team)
	if !narrowed.Super {
		t.Fatal("scoping must not erase the account's Super fact; only the capabilities must narrow on it")
	}

	if lvl, err := f.check.ProjectLevel(ctx, narrowed, f.project); err != nil || lvl != None {
		t.Fatalf("ProjectLevel scoped = %v, %v, want None", lvl, err)
	}
	if lvl, err := f.check.ProjectLevel(ctx, narrowed, f.projectOther); err != nil || lvl != None {
		t.Fatalf("ProjectLevel scoped (other project) = %v, %v, want None", lvl, err)
	}
	if _, err := f.check.RequireProject(ctx, narrowed, f.project, Member); StatusOf(err, 0) != 404 {
		t.Fatalf("RequireProject scoped = %v, want a 404", err)
	}

	// VisibleProjects: the empty set, and decisively not nil (nil = every
	// project, which the SSE filter would take at its word).
	visible, err := f.check.VisibleProjects(ctx, narrowed)
	if err != nil {
		t.Fatalf("VisibleProjects scoped: %v", err)
	}
	if visible == nil {
		t.Fatal("VisibleProjects scoped returned nil, which means ALL projects in its contract")
	}
	if len(visible) != 0 {
		t.Fatalf("VisibleProjects scoped = %v, want the empty set", visible)
	}

	if role, err := f.check.OrgRole(ctx, narrowed, f.org); err != nil || role != "" {
		t.Fatalf("OrgRole scoped = %q, %v, want no role", role, err)
	}
	if _, err := f.check.RequireOrgMember(ctx, narrowed, f.org); CodeOf(err) != CodeTokenScopeDenied {
		t.Fatalf("RequireOrgMember scoped = %v, want %s", err, CodeTokenScopeDenied)
	}
	if _, err := f.check.RequireOrgAdmin(ctx, narrowed, f.org); CodeOf(err) != CodeTokenScopeDenied {
		t.Fatalf("RequireOrgAdmin scoped = %v, want %s", err, CodeTokenScopeDenied)
	}

	// The inbox: not readable, its own team's included.
	if role, err := f.check.InboxRole(ctx, narrowed, f.team); err != nil || role != InboxNone {
		t.Fatalf("InboxRole scoped (own team) = %v, %v, want InboxNone", role, err)
	}
	if role, err := f.check.InboxRole(ctx, narrowed, f.otherTeam); err != nil || role != InboxNone {
		t.Fatalf("InboxRole scoped (other team) = %v, %v, want InboxNone", role, err)
	}

	// Team management, its own team's included: managing the inbox would let
	// the credential hand itself more than submitting.
	team, err := f.repo.GetTeam(ctx, f.team)
	if err != nil {
		t.Fatalf("load team: %v", err)
	}
	if err := f.check.RequireTeamManager(ctx, narrowed, team); CodeOf(err) != CodeTokenScopeDenied {
		t.Fatalf("RequireTeamManager scoped (own team) = %v, want %s", err, CodeTokenScopeDenied)
	}
}

// TestSubmitScopeTeamSubmitIsTheOnlyShape is the submit half: the bound team
// passes while the account could submit there, another team is indistinguishable
// from one that does not exist, and losing the account's own standing takes the
// credential's ability with it.
func TestSubmitScopeTeamSubmitIsTheOnlyShape(t *testing.T) {
	f := newSubmitFixture(t)
	ctx := f.ctx

	// A customer whose only path to the inbox is a submit grant: the credential
	// narrows exactly the account a service connector would hold.
	f.grant(f.team, f.cust, f.lead)
	own := scoped(f.principal(f.cust), f.team)

	if err := f.check.TeamSubmit(ctx, own, f.team); err != nil {
		t.Fatalf("a scoped credential bound to the team its account may submit to was refused: %v", err)
	}

	// Not another team in the same org, and the refusal is the not-found one:
	// a narrowed credential learns nothing about teams it was not bound to.
	if err := f.check.TeamSubmit(ctx, own, f.otherTeam); StatusOf(err, 0) != 404 {
		t.Fatalf("TeamSubmit scoped (other team) = %v, want a 404", err)
	}
	// Not even by being rebound on the fly: a credential bound to another team
	// may not submit to this one…
	if err := f.check.TeamSubmit(ctx, scoped(f.principal(f.cust), f.otherTeam), f.team); StatusOf(err, 0) != 404 {
		t.Fatalf("TeamSubmit scoped (rebound) = %v, want a 404", err)
	}
	// …and a credential bound to the team still does not pass for an account
	// that may not submit there.
	if err := f.check.TeamSubmit(ctx, scoped(f.principal(f.ungranted), f.team), f.team); StatusOf(err, 0) == 0 {
		t.Fatal("a scoped credential submitted for an account with no standing on the team")
	}

	// The account's standing is re-read every call: revoking the grant stops
	// the credential on the very next one, with no cache to reset.
	f.revoke(f.team, f.cust, f.lead)
	if err := f.check.TeamSubmit(ctx, own, f.team); StatusOf(err, 0) == 0 {
		t.Fatal("a scoped credential still submitted after its account's grant was revoked")
	}
	f.grant(f.team, f.cust, f.lead)
	if err := f.check.TeamSubmit(ctx, own, f.team); err != nil {
		t.Fatalf("a scoped credential did not come back when its account's grant was restored: %v", err)
	}

	// A global admin's scoped credential is still bound: Super does not pierce
	// the binding, and an account that administers every team still submits
	// only where its credential says.
	superTok := scoped(Principal{UserID: f.admin, Super: true}, f.team)
	if err := f.check.TeamSubmit(ctx, superTok, f.team); err != nil {
		t.Fatalf("a scoped admin credential was refused its own team: %v", err)
	}
	if err := f.check.TeamSubmit(ctx, superTok, f.otherTeam); StatusOf(err, 0) != 404 {
		t.Fatalf("a scoped admin credential reached another team: %v", err)
	}
}

// TestSubmitScopeFullIsUnchanged is the regression guard the ticket's second
// criterion names: an unscoped token, and a principal with no scope at all (a
// Dex session), answer every question exactly as before. Without this, a guard
// that refused too much would look like a passing security test.
func TestSubmitScopeFullIsUnchanged(t *testing.T) {
	f := newSubmitFixture(t)
	ctx := f.ctx
	f.grant(f.team, f.cust, f.lead)

	// The full-scope versions of the principals the narrowing test uses.
	fullAdmin := Principal{UserID: f.admin, Super: true, Scope: domain.TokenScopeFull}
	noneAdmin := Principal{UserID: f.admin, Super: true} // pre-scope / Dex session
	for _, p := range []Principal{fullAdmin, noneAdmin} {
		if lvl, err := f.check.ProjectLevel(ctx, p, f.project); err != nil || lvl != Super {
			t.Fatalf("ProjectLevel(%+v) = %v, %v, want Super", p, lvl, err)
		}
		visible, err := f.check.VisibleProjects(ctx, p)
		if err != nil || visible != nil { // nil = all, the pre-scope behavior
			t.Fatalf("VisibleProjects(%+v) = %v, %v, want nil (all)", p, visible, err)
		}
		if role, err := f.check.OrgRole(ctx, p, f.org); err != nil || role != domain.OrgRoleOwner {
			t.Fatalf("OrgRole(%+v) = %q, %v, want owner", p, role, err)
		}
		if err := f.check.TeamSubmit(ctx, p, f.team); err != nil {
			t.Fatalf("TeamSubmit(%+v, own team) = %v, want pass", p, err)
		}
		if err := f.check.TeamSubmit(ctx, p, f.otherTeam); err != nil {
			t.Fatalf("TeamSubmit(%+v, other team) = %v; an unscoped admin submits anywhere", p, err)
		}
		if role, err := f.check.InboxRole(ctx, p, f.otherTeam); err != nil || role != InboxMember {
			t.Fatalf("InboxRole(%+v) = %v, %v, want InboxMember", p, role, err)
		}
	}
	// A full-scoped token on the granted customer is still just that customer:
	// the scope adds nothing, and the grant is what decides.
	plainCust := Principal{UserID: f.cust, Scope: domain.TokenScopeFull}
	if err := f.check.TeamSubmit(ctx, plainCust, f.team); err != nil {
		t.Fatalf("TeamSubmit(full customer with a grant) = %v, want pass", err)
	}
	if err := f.check.TeamSubmit(ctx, plainCust, f.otherTeam); StatusOf(err, 0) != 404 {
		t.Fatalf("TeamSubmit(full customer, other team) = %v, want a 404", err)
	}
	if role, err := f.check.InboxRole(ctx, plainCust, f.team); err != nil || role != InboxNone {
		t.Fatalf("InboxRole(full customer with a grant) = %v, %v, want InboxNone (a grant is not reading)", role, err)
	}
}

// TestSubmitScopeIdentityComesFromTheToken pins the resolution path: the scope
// a Principal carries comes from the token row (auth.Identity), and an identity
// with no scope — a Dex session, an in-process caller — is full.
func TestSubmitScopeIdentityComesFromTheToken(t *testing.T) {
	teamID := 42
	id := auth.Identity{APIToken: true, TokenID: 9, TokenScope: domain.TokenScopeSubmit, TokenScopeTeamID: &teamID}
	got, ok := auth.FromContext(auth.WithIdentity(context.Background(), &id))
	if !ok {
		t.Fatal("identity not found on the context")
	}
	if got.TokenScope != domain.TokenScopeSubmit || got.TokenScopeTeamID == nil || *got.TokenScopeTeamID != teamID {
		t.Fatalf("identity scope = %q/%v, want submit/%d", got.TokenScope, got.TokenScopeTeamID, teamID)
	}
	if !(Principal{Scope: got.TokenScope, ScopeTeamID: *got.TokenScopeTeamID}).AllowsSubmitScope(teamID) {
		t.Fatal("a principal built from a submit-scoped identity may not submit")
	}
	// The same helper the middleware uses: anything that is not the explicit
	// submit scope is full, so an empty scope on any other identity is unchanged.
	if s := tokenScopeOrFull(""); s != domain.TokenScopeFull {
		t.Fatalf("tokenScopeOrFull(\"\") = %q, want full", s)
	}
	if s := tokenScopeOrFull(domain.TokenScopeSubmit); s != domain.TokenScopeSubmit {
		t.Fatalf("tokenScopeOrFull(submit) = %q", s)
	}
	if s := tokenScopeOrFull("a-future-scope"); s != domain.TokenScopeFull {
		t.Fatalf("tokenScopeOrFull(a-future-scope) = %q; an unknown value must not narrow (nor break) anything", s)
	}
	if n := tokenScopeTeam(nil); n != 0 {
		t.Fatalf("tokenScopeTeam(nil) = %d, want 0", n)
	}
}
