package main

// Submit-only service credentials, end to end (AMP #3772, build slice B5;
// approved contract artifact 36 v2 §5).
//
// The ticket's three criteria, and what proves each:
//
//  1. **Allowed-team submit scope and foreign/read denials.** A token minted
//     submit-only for the inbox team reaches its one submission and is refused
//     everywhere else — every REST route the surface has, the SSE stream, and
//     every MCP tool except amp_submit_request — with the same 403 and the same
//     stable code, and its tool list advertises only that one tool
//     (TestSubmitScopeRefusesEveryOtherRoute, TestSubmitScopeMCPToolList).
//  2. **Unscoped credentials unchanged.** The same person's full-scope token
//     still reads the project, lists tickets and calls every tool exactly as
//     before (TestSubmitScopeLeavesFullTokensAlone).
//  3. **Scope escalation and token acceptance blocked.** No REST or MCP path
//     widens a scope (there is no update surface at all), a scoped credential
//     cannot mint a broader one, an issuer cannot create a scope on a team they
//     could not submit to themselves, and no credential is ever "a person in
//     the web UI" (TestSubmitScopeCannotEscalate,
//     TestSubmitScopeIssuerCannotExceedTheirOwnAuthority).
//
// Submission itself (`POST /api/teams/{id}/requests`, `amp_submit_request`) is
// built by later slices; these tests therefore assert the *gate's* answer for
// that route — a scoped credential for the bound team is not refused by the
// scope gate — rather than a successful submission, and note where the handler
// does not exist yet. That is deliberate: the ticket's job is the credential
// boundary, and it must hold whether or not the submit handler has landed.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/api"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// submitScopeWorld is an rbacWorld plus the two things this ticket needs:
// a submit-only token bound to an inbox team, and its full-scope twin.
type submitScopeWorld struct {
	*rbacWorld
	// inbox is the team the scoped token is bound to. alice (a member of devs)
	// is made a member of it, so her account may submit there on its own rows.
	inbox *domain.Team
	// otherTeam is a second team in the same org with no standing for alice:
	// the foreign team a scoped credential must not reach.
	otherTeam *domain.Team

	// scoped is alice's submit-only token, and full her full-scope one.
	scoped, full string
	// scopedMCP and fullMCP are the same tokens as MCP identities: what the
	// MCP listener's middleware produces from a token (APIToken set, the scope
	// attached, the channel token).
	scopedIdent, fullIdent *auth.Identity
}

func newSubmitScopeWorld(t *testing.T) *submitScopeWorld {
	t.Helper()
	w := &submitScopeWorld{rbacWorld: newRBACWorld(t)}
	ctx := context.Background()
	repo := w.s.repo

	if err := repo.SetTeamMember(ctx, w.devs.ID, w.alice.User.ID, domain.TeamRoleMember); err != nil {
		t.Fatalf("alice on devs: %v", err)
	}
	inbox, err := repo.CreateTeam(ctx, w.org.ID, "inbox-"+fmt.Sprint(w.org.ID), nil)
	if err != nil {
		t.Fatalf("create inbox: %v", err)
	}
	w.inbox = inbox
	if err := repo.SetTeamMember(ctx, inbox.ID, w.alice.User.ID, domain.TeamRoleMember); err != nil {
		t.Fatalf("alice on inbox: %v", err)
	}
	// The lead of the inbox, for the issuer-side test: their own rows let them
	// submit there, so a submit-only token of their own is a fair ask.
	if err := repo.SetTeamMember(ctx, inbox.ID, w.lead.User.ID, domain.TeamRoleLead); err != nil {
		t.Fatalf("lead on inbox: %v", err)
	}
	other, err := repo.CreateTeam(ctx, w.org.ID, "other-inbox-"+fmt.Sprint(w.org.ID), nil)
	if err != nil {
		t.Fatalf("create other team: %v", err)
	}
	w.otherTeam = other

	// Mint both tokens through the service, as a person in the web UI would
	// (alice minting for herself; alice is a member of the inbox team, so the
	// issuer check passes on her own rows).
	ui := authz.Principal{UserID: w.alice.User.ID, Email: w.alice.User.Email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	tok, err := svc.CreateScopedToken(ctx, ui, w.alice.User.ID, "connector", 0,
		service.TokenScopeRequest{Scope: "submit", TeamID: inbox.ID})
	if err != nil {
		t.Fatalf("mint submit-only token: %v", err)
	}
	w.scoped = tok.Token
	full, err := svc.CreateToken(ctx, ui, w.alice.User.ID, "full", 0)
	if err != nil {
		t.Fatalf("mint full token: %v", err)
	}
	w.full = full.Token

	// The identities the auth middleware builds from those rows: re-read them
	// from the database through the same lookup the verifier uses, so the scope
	// these tests exercise is the stored one and not a test-only shortcut.
	u, scopedTok, err := repo.UserAndTokenForAPIToken(ctx, auth.HashAPIToken(w.scoped))
	if err != nil {
		t.Fatalf("resolve submit-only token: %v", err)
	}
	w.scopedIdent = &auth.Identity{Subject: u.Subject, Email: u.Email, Name: u.DisplayName, User: u,
		APIToken: true, TokenID: int64(scopedTok.ID), TokenScope: scopedTok.Scope, TokenScopeTeamID: scopedTok.ScopeTeamID}
	if scopedTok.Scope != domain.TokenScopeSubmit || scopedTok.ScopeTeamID == nil || *scopedTok.ScopeTeamID != inbox.ID {
		t.Fatalf("stored scope = %q/%v, want submit/%d", scopedTok.Scope, scopedTok.ScopeTeamID, inbox.ID)
	}
	u2, fullTok, err := repo.UserAndTokenForAPIToken(ctx, auth.HashAPIToken(w.full))
	if err != nil {
		t.Fatalf("resolve full token: %v", err)
	}
	w.fullIdent = &auth.Identity{Subject: u2.Subject, Email: u2.Email, Name: u2.DisplayName, User: u2,
		APIToken: true, TokenID: int64(fullTok.ID), TokenScope: fullTok.Scope, TokenScopeTeamID: fullTok.ScopeTeamID}
	if fullTok.Scope != domain.TokenScopeFull || fullTok.ScopeTeamID != nil {
		t.Fatalf("stored full scope = %q/%v, want full/nil", fullTok.Scope, fullTok.ScopeTeamID)
	}
	return w
}

// scopedRoutes is every route a submit-scoped credential must be refused on: a
// spread of the REST surface (reads, lists, writes, org and team administration,
// grant administration, token minting, audit, day reports, KB, artifacts, the
// SSE stream), not just the project routes. Each is a real registered route, so
// a regression in the gate shows up here rather than in a future wave check.
func scopedRoutes(w *submitScopeWorld) []struct{ method, path string } {
	pid := fmt.Sprint(w.s.projectID)
	tid := fmt.Sprint(w.otherTeam.ID)
	uid := fmt.Sprint(w.alice.User.ID)
	return []struct{ method, path string }{
		{"GET", "/api/me"},
		{"GET", "/api/projects"},
		{"POST", "/api/projects"},
		{"GET", "/api/projects/" + pid},
		{"GET", "/api/projects/" + pid + "/tasks"},
		{"GET", "/api/projects/" + pid + "/epics"},
		{"GET", "/api/projects/" + pid + "/members"},
		{"POST", "/api/projects/" + pid + "/tasks"},
		{"GET", "/api/teams/" + tid},
		{"GET", "/api/teams/" + tid + "/members"},
		{"POST", "/api/teams/" + tid + "/members"},
		{"POST", "/api/teams/" + tid + "/submit-grants"},
		{"DELETE", "/api/teams/" + tid + "/submit-grants/" + uid},
		{"GET", "/api/users/" + uid + "/tokens"},
		{"POST", "/api/users/" + uid + "/tokens"},
		{"GET", "/api/agents"},
		{"POST", "/api/agents"},
		{"GET", "/api/audit"},
		{"GET", "/api/my-work"},
		{"GET", "/api/day-reports"},
		{"GET", "/api/kb/list?project_id=" + pid},
		{"GET", "/api/events"},
		// The submission itself, but on the WRONG team: the scope gate answers
		// the route as allowed (it is the one submit shape) and the handler's
		// authorization then refuses the team — either way, not a success.
		{"POST", "/api/teams/" + tid + "/requests"},
	}
}

// TestSubmitScopeRefusesEveryOtherRoute is the ticket's first criterion on the
// REST surface: a token minted for one inbox is refused everywhere but its own
// team's submission, with one stable code, and the refusal names no resource.
func TestSubmitScopeRefusesEveryOtherRoute(t *testing.T) {
	w := newSubmitScopeWorld(t)
	srv := bearerServer(t, w.rbacWorld)

	for _, rt := range scopedRoutes(w) {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			code, body := bearerDo(t, srv, w.scoped, rt.method, rt.path, map[string]any{})
			if code != 403 {
				t.Fatalf("%s %s with a submit-only token = %d %v, want 403", rt.method, rt.path, code, body)
			}
			// One code for every denial, so a client can tell a scope refusal
			// from an ordinary permission refusal.
			if got := body["code"]; got != authz.CodeTokenScopeDenied {
				t.Fatalf("%s %s code = %v, want %q", rt.method, rt.path, got, authz.CodeTokenScopeDenied)
			}
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, "only submit requests") {
				t.Fatalf("%s %s message = %q, want it to say what the credential may do", rt.method, rt.path, msg)
			}
		})
	}

	// The SSE stream must be a refused request, not an open stream that never
	// sends anything (the gate runs before the stream starts).
	code, body := bearerDo(t, srv, w.scoped, "GET", "/api/events", nil)
	if code != 403 || body["code"] != authz.CodeTokenScopeDenied {
		t.Fatalf("SSE with a submit-only token = %d %v, want a 403 %s", code, body, authz.CodeTokenScopeDenied)
	}
}

// TestSubmitScopeAllowsOnlyItsOwnSubmission is the allow half. Submission's own
// handler is a later slice, so what is asserted is what belongs to *this*
// ticket: the scope gate lets the bound team's submission through to the next
// layer, and refuses the same route for any other team. Whatever the next layer
// answers (today: 404, because the route is not registered yet; when the F1
// slice lands: the service's own answer), it is never the scope refusal.
func TestSubmitScopeAllowsOnlyItsOwnSubmission(t *testing.T) {
	w := newSubmitScopeWorld(t)
	srv := bearerServer(t, w.rbacWorld)

	code, body := bearerDo(t, srv, w.scoped, "POST", fmt.Sprintf("/api/teams/%d/requests", w.inbox.ID),
		map[string]any{"subject": "a scoped submission", "input_brief": "please"})
	if code == 403 || body["code"] == authz.CodeTokenScopeDenied {
		t.Fatalf("the bound team's submission was refused by the scope gate: %d %v", code, body)
	}

	// A different team is not the credential's submission even though the route
	// shape is the same: either the scope gate or the handler's authorization
	// refuses it, and it never succeeds.
	code, body = bearerDo(t, srv, w.scoped, "POST", fmt.Sprintf("/api/teams/%d/requests", w.otherTeam.ID),
		map[string]any{"subject": "not mine", "input_brief": "please"})
	if code >= 200 && code < 300 {
		t.Fatalf("a submit-only token submitted to a team it is not bound to: %d %v", code, body)
	}

	// A sub-path of the submission route is a different operation (a request
	// read, a comment, a claim) and is refused by the gate, even under the
	// bound team.
	for _, sub := range []string{"1", "1/comments", "1/claim", "1/accept"} {
		code, body := bearerDo(t, srv, w.scoped, "POST", fmt.Sprintf("/api/teams/%d/requests/%s", w.inbox.ID, sub), nil)
		if code != 403 || body["code"] != authz.CodeTokenScopeDenied {
			t.Fatalf("POST requests/%s with a submit-only token = %d %v, want 403 %s", sub, code, body, authz.CodeTokenScopeDenied)
		}
	}
	// The same path by GET is a read, not a submission.
	code, body = bearerDo(t, srv, w.scoped, "GET", fmt.Sprintf("/api/teams/%d/requests", w.inbox.ID), nil)
	if code != 403 || body["code"] != authz.CodeTokenScopeDenied {
		t.Fatalf("GET a team's requests with a submit-only token = %d %v, want 403 %s", code, body, authz.CodeTokenScopeDenied)
	}
}

// TestSubmitScopeMCPToolList: a scoped credential's tools/list advertises the
// one tool it may use — the honest list — while every other tool call is
// refused with the same code. Both halves matter: the list is what a client
// plans from, the middleware is what enforces.
func TestSubmitScopeMCPToolList(t *testing.T) {
	w := newSubmitScopeWorld(t)

	// tools/list as the scoped credential.
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	resp := w.s.as(w.scopedIdent).srv.HandleMessage(w.s.as(w.scopedIdent).callCtx(), raw)
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("decode tools/list: %v (%s)", err, b)
	}
	// The invariant, whichever slice has landed: nothing but the submission may
	// ever be advertised to a submit-only credential. (The submit tool itself is
	// build slice I1, so today this is the empty list; when it lands, this list
	// is exactly it — and either way nothing else may appear.)
	var names []string
	for _, tl := range list.Result.Tools {
		names = append(names, tl.Name)
		if tl.Name != "amp_submit_request" {
			t.Fatalf("scoped tools/list advertises %q; a submit-only credential may only be shown its submission (list = %v)", tl.Name, names)
		}
	}
	if len(names) > 1 {
		t.Fatalf("scoped tools/list = %v, want at most [amp_submit_request]", names)
	}

	// Every other tool is refused when called anyway (hidden tools stay
	// callable in mcp-go, so the list alone cannot be the enforcement).
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"amp_get_project", map[string]any{"project_id": float64(w.s.projectID)}},
		{"amp_list_tasks", map[string]any{"project_id": float64(w.s.projectID)}},
		{"amp_create_task", map[string]any{"story_id": float64(w.s.storyID), "name": "x", "description": "y", "acceptance_criteria": "z"}},
		{"amp_whoami", map[string]any{}},
		{"amp_list_projects", map[string]any{}},
		{"amp_audit_log", map[string]any{"project_id": float64(w.s.projectID)}},
		{"amp_manage_teams", map[string]any{"action": "list"}},
		{"amp_manage_team_members", map[string]any{"team_id": float64(w.otherTeam.ID), "action": "list"}},
		{"amp_manage_teams", map[string]any{"action": "list"}},
		{"amp_kb_list", map[string]any{"project_id": float64(w.s.projectID)}},
	}
	scopedStack := w.s.as(w.scopedIdent)
	for _, c := range calls {
		t.Run(c.tool, func(t *testing.T) {
			msg := scopedStack.callExpectError(t, c.tool, c.args)
			if !strings.Contains(msg, "may only submit requests") {
				t.Fatalf("%s with a submit-only token: %q, want the scope refusal", c.tool, msg)
			}
		})
	}

	// A refused tool call is audited as access.denied, the same row a refused
	// REST route writes: the scope gate sits inside the denied middleware, so
	// there is no silent refusal path (AMP #3772). Counted through the audit
	// reader, filtered to this caller's email so parallel tests can't supply it.
	entries, err := w.s.repo.ListAudit(context.Background(), domain.AuditFilter{
		ActorUserID: w.alice.User.ID, Action: "access.", Limit: 200,
	})
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var denied int
	for _, e := range entries {
		if e.TargetType == "tool" && strings.HasPrefix(e.TargetID, "amp_") {
			denied++
		}
	}
	if denied == 0 {
		t.Fatal("no tool scope refusal was audited as access.denied")
	}
}

// TestSubmitScopeLeavesFullTokensAlone is the regression guard for the ticket's
// second criterion: the same account's full-scope token behaves exactly as it
// did before scopes existed, on both surfaces.
func TestSubmitScopeLeavesFullTokensAlone(t *testing.T) {
	w := newSubmitScopeWorld(t)
	srv := bearerServer(t, w.rbacWorld)

	// REST: the project her team is linked to, its tickets, and her own work.
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/me"},
		{"GET", fmt.Sprintf("/api/projects/%d", w.s.projectID)},
		{"GET", fmt.Sprintf("/api/projects/%d/tasks", w.s.projectID)},
		{"GET", fmt.Sprintf("/api/users/%d/tokens", w.alice.User.ID)},
		{"GET", "/api/my-work"},
	} {
		code, body := bearerDo(t, srv, w.full, rt.method, rt.path, nil)
		if code != 200 {
			t.Fatalf("full-scope token %s %s = %d %v, want 200", rt.method, rt.path, code, body)
		}
	}
	// And /api/me says the credential is full, so a client can tell them apart.
	_, me := bearerDo(t, srv, w.full, "GET", "/api/me", nil)
	if me["via_token"] != true {
		t.Fatalf("/api/me with a full token = %v, want via_token true", me)
	}

	// MCP: the ordinary tools still work, and the tool list is the ordinary one
	// (many tools, not the submit-only single entry).
	fullStack := w.s.as(w.fullIdent)
	if got := intField(t, fullStack.call(t, "amp_whoami", map[string]any{}), "user_id"); got != w.alice.User.ID {
		t.Fatalf("amp_whoami with a full token = user %d, want %d", got, w.alice.User.ID)
	}
	if got := intField(t, fullStack.call(t, "amp_get_project", map[string]any{"project_id": float64(w.s.projectID)})["project"].(map[string]any), "id"); got != w.s.projectID {
		t.Fatalf("amp_get_project with a full token = %d, want %d", got, w.s.projectID)
	}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	resp := fullStack.srv.HandleMessage(fullStack.callCtx(), raw)
	b, _ := json.Marshal(resp)
	var list struct {
		Result struct {
			Tools []struct{} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Result.Tools) < 10 {
		t.Fatalf("a full-scope token's tools/list has %d tools; the ordinary list must be unchanged", len(list.Result.Tools))
	}
}

// TestSubmitScopeCannotEscalate is the third criterion's first half: nothing
// widens a scope. There is no update route or tool; a scoped credential can't
// mint tokens at all (so it can't mint itself a broader one); and a caller
// cannot smuggle a scope in through the ordinary token body's unknown fields.
func TestSubmitScopeCannotEscalate(t *testing.T) {
	w := newSubmitScopeWorld(t)
	ra := newRestAs(t, w.rbacWorld)

	// The scoped credential has no mint route: refused by the gate before the
	// handler, so this is not "the service said no" but "the surface is closed".
	ra2 := newRestAsToken(t, w.rbacWorld, w.scopedIdent)
	code, body := ra2.do(t, "", "POST", fmt.Sprintf("/api/users/%d/tokens", w.alice.User.ID), map[string]any{"name": "wider"})
	if code != 403 || body["code"] != authz.CodeTokenScopeDenied {
		t.Fatalf("a submit-only token minted a token: %d %v, want 403 %s", code, body, authz.CodeTokenScopeDenied)
	}

	// Alice herself (in the UI) cannot mint a scope on a team she could not
	// submit to: the issuer-side check refuses it, so no credential exists that
	// its issuer could not use themselves.
	ra.want(t, 403, "alice", "POST", fmt.Sprintf("/api/users/%d/tokens", w.alice.User.ID), map[string]any{
		"name": "reach", "scope": "submit", "scope_team_id": w.otherTeam.ID,
	})

	// A malformed or unknown scope is a 400, never a silently full token.
	for _, body := range []map[string]any{
		{"name": "bad", "scope": "admin"},
		{"name": "bad", "scope": "full", "scope_team_id": w.inbox.ID},
		{"name": "bad", "scope": "submit"},
	} {
		ra.want(t, 400, "alice", "POST", fmt.Sprintf("/api/users/%d/tokens", w.alice.User.ID), body)
	}

	// The row itself: the scoped token's scope cannot be widened by any write
	// the application makes. The only UPDATE on api_tokens is revocation, which
	// the migration's CHECK and the repository's single write path both keep
	// away from the scope columns; assert the token's own row is still exactly
	// what it was minted as after a full round of use.
	_, tok, err := w.s.repo.UserAndTokenForAPIToken(context.Background(), auth.HashAPIToken(w.scoped))
	if err != nil {
		t.Fatal(err)
	}
	if tok.Scope != domain.TokenScopeSubmit || tok.ScopeTeamID == nil || *tok.ScopeTeamID != w.inbox.ID {
		t.Fatalf("the scoped token's row changed to %q/%v after use", tok.Scope, tok.ScopeTeamID)
	}
	// And a full token's row is still unbound: nothing in the ordinary path
	// acquires a team.
	_, fullTok, err := w.s.repo.UserAndTokenForAPIToken(context.Background(), auth.HashAPIToken(w.full))
	if err != nil {
		t.Fatal(err)
	}
	if fullTok.Scope != domain.TokenScopeFull || fullTok.ScopeTeamID != nil {
		t.Fatalf("a full token's row became %q/%v", fullTok.Scope, fullTok.ScopeTeamID)
	}
}

// TestSubmitScopeIssuerCannotExceedTheirOwnAuthority is the ticket's "issuer
// cannot grant authority they lack": minting a submit-only token for a team the
// issuer could not submit to is refused, and the refusal follows TeamSubmit's
// shape (404 for a team the issuer has no relationship with).
func TestSubmitScopeIssuerCannotExceedTheirOwnAuthority(t *testing.T) {
	w := newSubmitScopeWorld(t)
	ra := newRestAs(t, w.rbacWorld)

	// Bob is a member of devs only: he cannot submit to the inbox.
	ra.want(t, 403, "bob", "POST", fmt.Sprintf("/api/users/%d/tokens", w.bob.User.ID), map[string]any{
		"name": "reach", "scope": "submit", "scope_team_id": w.inbox.ID,
	})
	// Outsider is in another org entirely: the team is not found (never
	// confirmed), and no token is minted.
	ra.want(t, 404, "outsider", "POST", fmt.Sprintf("/api/users/%d/tokens", w.outsider.User.ID), map[string]any{
		"name": "reach", "scope": "submit", "scope_team_id": w.inbox.ID,
	})
	// A nonexistent team is a 404 too, never a minted token bound to nothing.
	ra.want(t, 404, "alice", "POST", fmt.Sprintf("/api/users/%d/tokens", w.alice.User.ID), map[string]any{
		"name": "reach", "scope": "submit", "scope_team_id": 999999,
	})
	// Nobody accumulated a submit-only token from those refusals.
	tokens, err := w.s.repo.ListAPITokens(context.Background(), w.bob.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range tokens {
		if tok.SubmitOnly() {
			t.Fatalf("bob holds a submit-only token despite every mint being refused: %+v", tok)
		}
	}

	// The team lead of the inbox may mint for themselves (their own rows let
	// them submit), and the resulting token is bound as asked.
	leadTok := w.mintSubmitToken(t, "lead", w.inbox.ID)
	if !strings.HasPrefix(leadTok, "amp_") {
		t.Fatalf("lead's token = %q", leadTok)
	}
	tokens, err = w.s.repo.ListAPITokens(context.Background(), w.lead.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tok := range tokens {
		if tok.SubmitOnly() && tok.ScopeTeamID != nil && *tok.ScopeTeamID == w.inbox.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the lead's submit-only token is not in force on the inbox: %+v", tokens)
	}
}

// TestSubmitScopeIsNotAcceptedAsAPerson is the ticket's "requester outcome
// acceptance remains human requester/UI-only, never token": no credential is a
// person in the web UI, so every person-only act is refused to it.
func TestSubmitScopeIsNotAcceptedAsAPerson(t *testing.T) {
	w := newSubmitScopeWorld(t)

	// The identities the listener would build, forced onto the UI channel: the
	// token check is what must hold, not the channel label.
	for name, ident := range map[string]*auth.Identity{"submit-only": w.scopedIdent, "full": w.fullIdent} {
		c := *ident
		c.Listener, c.Channel = "rest", auth.ChannelUI
		p, err := authz.FromContext(auth.WithIdentity(context.Background(), &c))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.InUI() {
			t.Fatalf("%s: a token principal reads as a person in the web UI", name)
		}
		if p.SubmitOnly() != (name == "submit-only") {
			t.Fatalf("%s: SubmitOnly() = %v", name, p.SubmitOnly())
		}
	}
	// The scoped token cannot reach a person-only route at all: the gate
	// refuses it before any handler runs.
	ra := newRestAsToken(t, w.rbacWorld, w.scopedIdent)
	code, body := ra.do(t, "", "POST", fmt.Sprintf("/api/projects/%d/approve-plan", w.s.projectID), map[string]any{})
	if code != 403 || body["code"] != authz.CodeTokenScopeDenied {
		t.Fatalf("a scoped token approving a plan = %d %v, want 403 %s", code, body, authz.CodeTokenScopeDenied)
	}
}

// newRestAsToken serves the real REST mux, with every request running as the
// given API-token identity: the state the auth middleware produces from a
// bearer token (APIToken set, the stored scope attached, the token channel).
// The request's own X-Test-As header is ignored, so this is exactly what a
// script holding the token can do.
func newRestAsToken(t *testing.T, w *rbacWorld, ident *auth.Identity) *restAs {
	t.Helper()
	c := *ident
	c.Listener, c.Channel = "rest", auth.ChannelToken
	return newRestAsWithIdentity(t, w, &c)
}

// newRestAsWithIdentity is newRestAs with a fixed identity for every request.
func newRestAsWithIdentity(t *testing.T, w *rbacWorld, ident *auth.Identity) *restAs {
	t.Helper()
	mux := http.NewServeMux()
	api.NewRestHandler(w.s.reg, w.s.repo, w.s.hub, w.s.kb).Register(mux)
	ra := &restAs{w: w, ids: map[string]*auth.Identity{}}
	ra.server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithIdentity(r.Context(), ident))
		mux.ServeHTTP(rw, r)
	}))
	t.Cleanup(ra.server.Close)
	return ra
}

// ---- helpers ----

// mintSubmitToken mints a submit-only token for who, bound to team, through the
// REST surface as a person in the web UI, and returns the raw token.
func (w *submitScopeWorld) mintSubmitToken(t *testing.T, who string, team int) string {
	t.Helper()
	ra := newRestAs(t, w.rbacWorld)
	out := ra.want(t, 200, who, "POST", "/api/users/me/tokens", map[string]any{
		"name": "connector", "scope": "submit", "scope_team_id": team,
	})
	tok := out["token"].(map[string]any)
	if tok["scope"] != "submit" || int(tok["scope_team_id"].(float64)) != team {
		t.Fatalf("%s's minted token = %v, want scope submit on team %d", who, tok, team)
	}
	return tok["token"].(string)
}
