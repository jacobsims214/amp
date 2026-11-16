package main

// Minting a submit-only token, at the service layer (AMP #3772, build slice B5;
// approved contract artifact 36 v2 §5).
//
// The REST and MCP tests in token_scope_test.go prove the credential boundary;
// this one proves the two things that happen at mint time and nowhere else:
//
//   - **An issuer cannot grant authority they lack.** A submit-only token for
//     team T is minted only if the *issuer* may submit to T on their own rows
//     (Checker.TeamSubmit). A plain member of a peer team, a plain org member,
//     a stranger from another org and an unknown team are all refused, and a
//     refusal writes no token row.
//   - **The row is what the caller asked for, and the audit row names the scope
//     and never a credential.** A submit token stores scope=submit plus its
//     team; a token minted with no scope stores full and no team (exactly what
//     every pre-scope token has); and the token.create audit row carries the
//     scope, the team id and the token's database id — never the raw token, its
//     hash, or its display prefix's secret characters.
//
// The schema's own share (the closed vocabulary, the two CHECK implications, the
// team FK and its cascade) is in api_token_scopes_migration_test.go.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// tokenAuditRows returns the token.* audit rows, oldest first.
func (d *grantDB) tokenAuditRows() []auditRow {
	d.t.Helper()
	rows, err := d.db.Query(d.ctx, `
		SELECT action, actor_user_id, actor_email, actor_type, on_behalf_of_user_id, client,
		       target_type, target_id, coalesce(target_name, ''), outcome, details, "before", "after", org_id
		FROM audit_log WHERE action LIKE 'token.%' ORDER BY id`)
	if err != nil {
		d.t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var details, before, after []byte
		if err := rows.Scan(&r.action, &r.actorUserID, &r.actorEmail, &r.actorType, &r.onBehalfOf, &r.client,
			&r.targetType, &r.targetID, &r.targetName, &r.outcome, &details, &before, &after, &r.orgID); err != nil {
			d.t.Fatalf("scan audit: %v", err)
		}
		decode := func(b []byte) map[string]any {
			if len(b) == 0 {
				return nil
			}
			m := map[string]any{}
			if err := json.Unmarshal(b, &m); err != nil {
				d.t.Fatalf("audit json: %v", err)
			}
			return m
		}
		r.details, r.before, r.aftr = decode(details), decode(before), decode(after)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		d.t.Fatalf("audit rows: %v", err)
	}
	return out
}

// tokenRowsOf returns the token rows belonging to userID.
func (d *grantDB) tokenRowsOf(userID int) []domain.APIToken {
	d.t.Helper()
	toks, err := d.repo().ListAPITokens(d.ctx, userID)
	if err != nil {
		d.t.Fatalf("tokens of %d: %v", userID, err)
	}
	return toks
}

func TestCreateScopedToken(t *testing.T) {
	t.Run("a submit-only token stores its scope and team", func(t *testing.T) {
		d := newGrantDB(t)
		svc := d.svc()
		// The lead administers the inbox, so their own rows let them submit.
		tok, err := svc.CreateScopedToken(d.ctx, d.principal(d.lead), d.lead, "connector", 0,
			service.TokenScopeRequest{Scope: "submit", TeamID: d.team})
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if !strings.HasPrefix(tok.Token, "amp_") {
			t.Fatalf("token = %q, want an amp_ token", tok.Token)
		}
		if tok.Scope != domain.TokenScopeSubmit || tok.ScopeTeamID == nil || *tok.ScopeTeamID != d.team {
			t.Fatalf("minted row = scope %q team %v, want submit/%d", tok.Scope, tok.ScopeTeamID, d.team)
		}
		if !tok.SubmitOnly() {
			t.Fatal("the minted token does not read as submit-only")
		}

		// The stored row is the same, and the token resolves to it: the scope a
		// request will see is the one minted, read back through the bearer path.
		rows := d.tokenRowsOf(d.lead)
		if len(rows) != 1 || rows[0].Scope != domain.TokenScopeSubmit || rows[0].ScopeTeamID == nil || *rows[0].ScopeTeamID != d.team {
			t.Fatalf("stored tokens = %+v", rows)
		}

		// The audit row names the scope and the team, and nothing that could be
		// used as a credential.
		audits := d.tokenAuditRows()
		if len(audits) != 1 {
			t.Fatalf("%d token audit rows, want 1: %+v", len(audits), audits)
		}
		a := audits[0]
		if a.action != "token.create" || a.targetType != "user" || a.targetID != itoa(d.lead) {
			t.Fatalf("audit row = %+v", a)
		}
		if a.details["scope"] != domain.TokenScopeSubmit {
			t.Fatalf("audit details scope = %v, want submit", a.details["scope"])
		}
		if team, ok := a.details["scope_team_id"]; !ok || int(team.(float64)) != d.team {
			t.Fatalf("audit details scope_team_id = %v, want %d", a.details["scope_team_id"], d.team)
		}
		// The summary names the credential by its display prefix (the same
		// recognition label ListTokens shows) and the team, so a person reading
		// the audit knows which token and where it may be used — and never the
		// token itself, nor its stored hash.
		var summary string
		if err := d.db.QueryRow(d.ctx, `SELECT summary FROM audit_log WHERE action = 'token.create' ORDER BY id DESC LIMIT 1`).Scan(&summary); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(summary, tok.Prefix) {
			t.Fatalf("audit summary %q does not name the token's display prefix %q", summary, tok.Prefix)
		}
		if !strings.Contains(summary, "bound to team") || !strings.Contains(summary, itoa(d.team)) {
			t.Fatalf("audit summary %q does not name the bound team %d", summary, d.team)
		}
		blob := mustJSON(a) + summary
		for _, secret := range []string{tok.Token, auth.HashAPIToken(tok.Token)} {
			if secret != "" && strings.Contains(blob, secret) {
				t.Fatalf("the audit row contains a credential secret (%q): %s", secret, blob)
			}
		}
		// The raw token exists exactly once: in the response to the minting
		// caller. It is nowhere in the database.
		var stored int
		if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM api_tokens WHERE token_hash = $1 OR name = $2`,
			tok.Token, tok.Token).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != 0 {
			t.Fatalf("the raw token was stored %d times in api_tokens", stored)
		}
	})

	t.Run("no scope means full, exactly as before", func(t *testing.T) {
		d := newGrantDB(t)
		svc := d.svc()
		// CreateToken is the pre-scope entry point every existing caller uses.
		tok, err := svc.CreateToken(d.ctx, d.principal(d.lead), d.lead, "ordinary", 0)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if tok.Scope != domain.TokenScopeFull || tok.ScopeTeamID != nil || tok.SubmitOnly() {
			t.Fatalf("ordinary token = scope %q team %v, want full/nil", tok.Scope, tok.ScopeTeamID)
		}
		// An explicit "full" is the same row, and asking for a team with it is a
		// 400 rather than a silently bound token.
		full, err := svc.CreateScopedToken(d.ctx, d.principal(d.lead), d.lead, "explicit", 0,
			service.TokenScopeRequest{Scope: "full"})
		if err != nil || full.Scope != domain.TokenScopeFull || full.ScopeTeamID != nil {
			t.Fatalf("explicit full = %+v, %v", full, err)
		}
		for _, req := range []service.TokenScopeRequest{
			{Scope: "full", TeamID: d.team},
			{Scope: "submit"}, // no team
			{Scope: "admin"},  // unknown
			{Scope: "submit", TeamID: d.team /* and a bad value below */},
		} {
			if req.Scope == "submit" && req.TeamID != 0 {
				continue // the case that must succeed, covered above
			}
			if _, err := svc.CreateScopedToken(d.ctx, d.principal(d.lead), d.lead, "bad", 0, req); err == nil {
				t.Fatalf("CreateScopedToken(%+v) succeeded, want a 400", req)
			} else if authz.StatusOf(err, 500) != 400 {
				t.Fatalf("CreateScopedToken(%+v) = %v (%d), want 400", req, err, authz.StatusOf(err, 500))
			}
		}
		// Nothing was minted by the refused calls.
		if rows := d.tokenRowsOf(d.lead); len(rows) != 2 {
			t.Fatalf("%d tokens after 2 successful mints, want 2 (refusals must not store rows): %+v", len(rows), rows)
		}
	})

	t.Run("the issuer must be able to submit there", func(t *testing.T) {
		d := newGrantDB(t)
		svc := d.svc()
		// lead2 leads another team in the same org: they can see the inbox team
		// but may not submit to it (403), so no token is minted for it.
		if _, err := svc.CreateScopedToken(d.ctx, d.principal(d.lead2), d.lead2, "reach", 0,
			service.TokenScopeRequest{Scope: "submit", TeamID: d.team}); authz.StatusOf(err, 500) != 403 {
			t.Fatalf("a lead of another team minted for the inbox: %v", err)
		}
		// A plain org member with no team role: same refusal.
		if _, err := svc.CreateScopedToken(d.ctx, d.principal(d.member2), d.member2, "reach", 0,
			service.TokenScopeRequest{Scope: "submit", TeamID: d.team}); authz.StatusOf(err, 500) != 403 {
			t.Fatalf("a plain org member minted for the inbox: %v", err)
		}
		// A stranger from another org learns nothing: the team is not found.
		if _, err := svc.CreateScopedToken(d.ctx, d.principal(d.stranger), d.stranger, "reach", 0,
			service.TokenScopeRequest{Scope: "submit", TeamID: d.team}); authz.StatusOf(err, 500) != 404 {
			t.Fatalf("a stranger minted for another org's inbox: %v", err)
		}
		// An unknown team, for a caller who does administer one: also 404.
		if _, err := svc.CreateScopedToken(d.ctx, d.principal(d.lead), d.lead, "reach", 0,
			service.TokenScopeRequest{Scope: "submit", TeamID: 999999}); authz.StatusOf(err, 500) != 404 {
			t.Fatalf("minting for an unknown team: %v", err)
		}
		// Not one row was written by any of those.
		for _, u := range []int{d.lead2, d.member2, d.stranger, d.lead} {
			if rows := d.tokenRowsOf(u); len(rows) != 0 {
				t.Fatalf("user %d holds %d tokens after refused mints: %+v", u, len(rows), rows)
			}
		}
		if audits := d.tokenAuditRows(); len(audits) != 0 {
			t.Fatalf("refused mints wrote %d token audit rows: %+v", len(audits), audits)
		}

		// The same caller may mint once they actually hold submit access: a
		// grant for the customer, minted by that customer for themselves.
		if _, err := svc.GrantSubmitAccess(d.ctx, d.principal(d.lead), d.team, "requester@grant.test"); err != nil {
			t.Fatalf("grant: %v", err)
		}
		cust := d.principal(d.customer)
		if _, err := svc.CreateScopedToken(d.ctx, cust, d.customer, "connector", 0,
			service.TokenScopeRequest{Scope: "submit", TeamID: d.team}); err != nil {
			t.Fatalf("a granted customer could not mint their own submit-only token: %v", err)
		}
	})

	t.Run("a narrowed credential cannot mint", func(t *testing.T) {
		d := newGrantDB(t)
		svc := d.svc()
		// The scoped principal's account may administer the team; the credential
		// still may not mint — that is how a scope would otherwise be widened
		// into a broader token by the thing it narrows.
		scoped := d.principal(d.lead)
		scoped.Scope, scoped.ScopeTeamID = domain.TokenScopeSubmit, d.team
		if _, err := svc.CreateScopedToken(d.ctx, scoped, d.lead, "wider", 0,
			service.TokenScopeRequest{Scope: "full"}); authz.CodeOf(err) != authz.CodeTokenScopeDenied {
			t.Fatalf("a submit-only credential minted a full token: %v", err)
		}
		if _, err := svc.CreateToken(d.ctx, scoped, d.lead, "wider", 0); authz.CodeOf(err) != authz.CodeTokenScopeDenied {
			t.Fatalf("a submit-only credential minted through CreateToken: %v", err)
		}
		if rows := d.tokenRowsOf(d.lead); len(rows) != 0 {
			t.Fatalf("%d tokens were minted by a narrowed credential: %+v", len(rows), rows)
		}
	})
}

// itoa is strconv.Itoa for the audit-row assertions above.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

var _ = context.Background
