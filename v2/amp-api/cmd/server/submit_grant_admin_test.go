package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/reqctx"
	"github.com/simstech/amp-api/internal/service"
)

// Submit-grant administration (AMP #3769, approved contract artifact 36 v2 §2
// and decision D14, build slice B2's service half). These tests prove the
// boundary the administration service owns, on a real database, against the
// repository and schema #3768 landed:
//
//   - a team lead and an org owner/admin can grant and revoke submit access; a
//     plain team member, a plain org member, a lead of a *different* team in the
//     same org, an agent account with no role of its own and a customer are all
//     refused, and a cross-org manager is refused without learning whether the
//     team exists;
//   - every grant and revocation is audited once, with the acting principal
//     (human or agent, and for an agent the person it acts for), the channel it
//     came through, the team as the target, and the recipient's id — never a
//     credential;
//   - granting changes no project visibility at all: a customer with a grant
//     still reaches zero projects, and no org or team membership row appears;
//   - a refused call writes no grant, no revocation and no audit row of its own
//     (the listener records it as access.denied, once).

// grantDB is a fresh database at the current migration level with one org, an
// inbox team linked to a project, a second team in the same org, a second org
// with its own team, and the principals each test needs.
type grantDB struct {
	t   *testing.T
	db  *pgx.Conn
	dsn string
	ctx context.Context
	org int
	// team is the inbox team grants are administered on; project is linked to
	// it and is what "project visibility" is measured against.
	team    int
	project int
	// otherTeam is another team in the same org (the same-org, wrong-team case).
	otherTeam int
	// otherOrg/otherTeam2 are a second tenant.
	otherOrg, otherTeam2 int

	// admin is an org owner, lead2 the lead of otherTeam, lead the lead of team,
	// member a plain team member, member2 a plain org member with no team role.
	admin, lead, lead2, member, member2 int
	// customer is a user of the customer kind (the usual grant recipient) and
	// agent an agent-kind account managed by lead with no team role.
	customer, agent int
	// stranger owns the other org.
	stranger int
}

func newGrantDB(t *testing.T) *grantDB {
	t.Helper()
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer repo.Close()
	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })
	d := &grantDB{t: t, db: db, dsn: dsn, ctx: ctx}
	d.org = d.insert(`INSERT INTO orgs (name, slug) VALUES ('Grant org', 'grant-org') RETURNING id`)
	d.team = d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Inbox') RETURNING id`, d.org)
	d.otherTeam = d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Other team') RETURNING id`, d.org)
	d.project = d.insert(`INSERT INTO projects (name, code, org_id) VALUES ('Delivered work', 'GRANT', $1) RETURNING id`, d.org)
	d.exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`, d.project, d.team)
	d.otherOrg = d.insert(`INSERT INTO orgs (name, slug) VALUES ('Other grant org', 'other-grant-org') RETURNING id`)
	d.otherTeam2 = d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Foreign inbox') RETURNING id`, d.otherOrg)

	d.admin = d.user("grant-admin", "admin@grant.test", domain.UserKindHuman)
	d.lead = d.user("grant-lead", "lead@grant.test", domain.UserKindHuman)
	d.lead2 = d.user("grant-lead2", "lead2@grant.test", domain.UserKindHuman)
	d.member = d.user("grant-member", "member@grant.test", domain.UserKindHuman)
	d.member2 = d.user("grant-member2", "member2@grant.test", domain.UserKindHuman)
	d.customer = d.user("grant-customer", "requester@grant.test", domain.UserKindCustomer)
	d.stranger = d.user("grant-stranger", "stranger@grant.test", domain.UserKindHuman)

	d.exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, d.org, d.admin)
	for _, u := range []int{d.lead, d.lead2, d.member, d.member2} {
		d.exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, d.org, u)
	}
	d.exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, d.otherOrg, d.stranger)
	d.exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, d.team, d.lead)
	d.exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, d.otherTeam, d.lead2)
	d.exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, d.team, d.member)
	// An agent account: its own users row, managed by the lead, deliberately
	// with no team role (the contract's "an agent may do what its person may do"
	// is not how AMP models agents — they hold their own access).
	d.agent = d.insert(`INSERT INTO users (subject, email, kind, managed_by) VALUES ('grant-agent', 'builder@grant.test', 'agent', $1) RETURNING id`, d.lead)
	return d
}

func (d *grantDB) insert(sql string, args ...any) int {
	d.t.Helper()
	var id int
	if err := d.db.QueryRow(d.ctx, sql, args...).Scan(&id); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (d *grantDB) exec(sql string, args ...any) {
	d.t.Helper()
	if _, err := d.db.Exec(d.ctx, sql, args...); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
}

func (d *grantDB) user(subject, email, kind string) int {
	d.t.Helper()
	return d.insert(`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`, subject, email, kind)
}

func (d *grantDB) repo() *repository.Repo {
	d.t.Helper()
	repo, err := repository.New(d.ctx, d.dsn)
	if err != nil {
		d.t.Fatalf("connect: %v", err)
	}
	d.t.Cleanup(repo.Close)
	return repo
}

// svc builds the service over the fixture; the registry and the knowledge base
// are nil-safe for these calls (the audit writer needs only the repository).
func (d *grantDB) svc() *service.Service {
	d.t.Helper()
	return service.New(d.repo(), nil, nil)
}

// principal is a signed-in caller for service methods: a person in the web UI,
// or the channel/auth method the test asks for.
func (d *grantDB) principal(userID int) authz.Principal {
	d.t.Helper()
	var email string
	if err := d.db.QueryRow(d.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		d.t.Fatalf("email of %d: %v", userID, err)
	}
	return authz.Principal{UserID: userID, Email: email, Channel: auth.ChannelUI, AuthMethod: auth.AuthDexSession}
}

// grantOf returns the stored (team, user) grant row, or nil when there is none.
func (d *grantDB) grantOf(teamID, userID int) *domain.TeamSubmitGrant {
	d.t.Helper()
	g, err := d.repo().GetTeamSubmitGrant(d.ctx, teamID, userID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		d.t.Fatalf("grant (%d,%d): %v", teamID, userID, err)
	}
	return g
}

func (d *grantDB) maySubmit(teamID, userID int) bool {
	d.t.Helper()
	ok, err := d.repo().HasTeamSubmitGrant(d.ctx, teamID, userID)
	if err != nil {
		d.t.Fatalf("has grant (%d,%d): %v", teamID, userID, err)
	}
	return ok
}

// auditRow is one audit_log row as the administration actions write them.
type auditRow struct {
	action       string
	actorUserID  *int
	actorEmail   string
	actorType    string
	onBehalfOf   *int
	client       string
	targetType   string
	targetID     string
	targetName   string
	outcome      string
	details      map[string]any
	before, aftr map[string]any
	orgID        *int
}

// grantAuditRows returns the team.submit_grant.* rows, oldest first.
func (d *grantDB) grantAuditRows() []auditRow {
	d.t.Helper()
	rows, err := d.db.Query(d.ctx, `
		SELECT action, actor_user_id, actor_email, actor_type, on_behalf_of_user_id, client,
		       target_type, target_id, coalesce(target_name, ''), outcome, details, "before", "after", org_id
		FROM audit_log WHERE action LIKE 'team.submit_grant.%' ORDER BY id`)
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

// memberships returns the user's org roles and team roles, sorted.
func (d *grantDB) memberships(userID int) (orgRoles, teamRoles []string) {
	d.t.Helper()
	rows, err := d.db.Query(d.ctx, `SELECT role FROM org_members WHERE user_id = $1 ORDER BY role`, userID)
	if err != nil {
		d.t.Fatal(err)
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			d.t.Fatal(err)
		}
		orgRoles = append(orgRoles, r)
	}
	rows.Close()
	rows, err = d.db.Query(d.ctx, `SELECT role FROM team_members WHERE user_id = $1 ORDER BY role`, userID)
	if err != nil {
		d.t.Fatal(err)
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			d.t.Fatal(err)
		}
		teamRoles = append(teamRoles, r)
	}
	rows.Close()
	return orgRoles, teamRoles
}

// projectAccess counts the projects the user can reach, through the same query
// the authorization layer uses.
func (d *grantDB) projectAccess(userID int) int {
	d.t.Helper()
	ids, err := d.repo().AccessibleProjectIDs(d.ctx, userID)
	if err != nil {
		d.t.Fatalf("accessible projects of %d: %v", userID, err)
	}
	return len(ids)
}

// ---- Who may administer ----

// TestSubmitGrantAuthorization: the manager gate (RequireTeamManager, D14) holds
// for every principal kind and channel the contract's actor table names, and a
// refusal leaves nothing behind.
func TestSubmitGrantAuthorization(t *testing.T) {
	t.Run("a team lead grants and revokes", func(t *testing.T) {
		d := newGrantDB(t)
		svc := d.svc()
		g, err := svc.GrantSubmitAccess(d.ctx, d.principal(d.lead), d.team, "requester@grant.test")
		if err != nil {
			t.Fatalf("team lead grant: %v", err)
		}
		if g.TeamID != d.team || g.OrgID != d.org || g.UserID != d.customer || g.GrantedBy == nil || *g.GrantedBy != d.lead {
			t.Fatalf("grant = %+v", g)
		}
		if !d.maySubmit(d.team, d.customer) {
			t.Fatal("the granted customer may not submit")
		}
		revoked, err := svc.RevokeSubmitAccess(d.ctx, d.principal(d.lead), d.team, "requester@grant.test")
		if err != nil {
			t.Fatalf("team lead revoke: %v", err)
		}
		if revoked.Active() || revoked.RevokedBy == nil || *revoked.RevokedBy != d.lead {
			t.Fatalf("revoked grant = %+v", revoked)
		}
		if d.maySubmit(d.team, d.customer) {
			t.Fatal("submit access survived its revocation")
		}
	})

	t.Run("an org owner grants too", func(t *testing.T) {
		d := newGrantDB(t)
		if _, err := d.svc().GrantSubmitAccess(d.ctx, d.principal(d.admin), d.team, "requester@grant.test"); err != nil {
			t.Fatalf("org owner grant: %v", err)
		}
		if !d.maySubmit(d.team, d.customer) {
			t.Fatal("an org owner's grant did not take effect")
		}
	})

	t.Run("the org admin can also run the administration view", func(t *testing.T) {
		d := newGrantDB(t)
		svc := d.svc()
		if _, err := svc.GrantSubmitAccess(d.ctx, d.principal(d.admin), d.team, "requester@grant.test"); err != nil {
			t.Fatal(err)
		}
		list, err := svc.ListSubmitGrants(d.ctx, d.principal(d.lead), d.team)
		if err != nil || len(list) != 1 {
			t.Fatalf("list = %+v, %v", list, err)
		}
		if list[0].UserID != d.customer || list[0].Email != "requester@grant.test" ||
			list[0].Kind != domain.UserKindCustomer || !list[0].Active() {
			t.Fatalf("listed grant = %+v", list[0])
		}
	})

	refused := []struct {
		name  string
		who   func(d *grantDB) authz.Principal
		team  func(d *grantDB) int
		wants string // a substring the refusal must carry, when it should name one
	}{
		{
			name: "a plain team member",
			who:  func(d *grantDB) authz.Principal { return d.principal(d.member) },
			team: func(d *grantDB) int { return d.team },
		},
		{
			name: "a plain org member with no team role",
			who:  func(d *grantDB) authz.Principal { return d.principal(d.member2) },
			team: func(d *grantDB) int { return d.team },
		},
		{
			name: "the lead of a different team in the same org",
			who:  func(d *grantDB) authz.Principal { return d.principal(d.lead2) },
			team: func(d *grantDB) int { return d.team },
		},
		{
			name: "a customer account",
			who:  func(d *grantDB) authz.Principal { return d.principal(d.customer) },
			team: func(d *grantDB) int { return d.team },
		},
		{
			name: "an agent account with no team role of its own",
			who: func(d *grantDB) authz.Principal {
				p := d.principal(d.agent)
				p.Agent = true
				p.ManagedBy = &d.lead
				p.Channel = auth.ChannelMCP
				p.AuthMethod = auth.AuthAPIToken
				return p
			},
			team: func(d *grantDB) int { return d.team },
		},
		{
			// A manager of another org learns nothing about this team, and is
			// refused as if it did not exist (never 403, never a cross-tenant grant).
			name:  "an owner of another org",
			who:   func(d *grantDB) authz.Principal { return d.principal(d.stranger) },
			team:  func(d *grantDB) int { return d.team },
			wants: "not found",
		},
	}
	for _, c := range refused {
		t.Run(c.name+" is refused and changes nothing", func(t *testing.T) {
			d := newGrantDB(t)
			svc := d.svc()
			_, err := svc.GrantSubmitAccess(d.ctx, c.who(d), c.team(d), "requester@grant.test")
			if err == nil {
				t.Fatal("a non-manager granted submit access")
			}
			if status := authz.StatusOf(err, 500); status != 403 && status != 404 {
				t.Fatalf("refusal status = %d (%v), want 403 or 404", status, err)
			}
			if c.wants != "" && !strings.Contains(err.Error(), c.wants) {
				t.Fatalf("refusal does not read as not-found: %v", err)
			}
			if g := d.grantOf(d.team, d.customer); g != nil {
				t.Fatalf("a refused grant wrote a row: %+v", g)
			}
			if rows := d.grantAuditRows(); len(rows) != 0 {
				t.Fatalf("a refused grant was audited as the change: %+v", rows)
			}
			// The same principal cannot revoke either, and cannot read the listing.
			if _, err := svc.RevokeSubmitAccess(d.ctx, c.who(d), c.team(d), "requester@grant.test"); err == nil {
				t.Fatal("a non-manager revoked submit access")
			}
			if _, err := svc.ListSubmitGrants(d.ctx, c.who(d), c.team(d)); err == nil {
				t.Fatal("a non-manager read the submit-grant listing")
			}
			if rows := d.grantAuditRows(); len(rows) != 0 {
				t.Fatalf("a refused action was audited as the change: %+v", rows)
			}
		})
	}

	t.Run("a cross-org team is not found even for that org's owner", func(t *testing.T) {
		d := newGrantDB(t)
		// The stranger owns otherOrg; granting on *this* org's team must not work,
		// and this org's admin must not reach the other org's team.
		if _, err := d.svc().GrantSubmitAccess(d.ctx, d.principal(d.admin), d.otherTeam2, "requester@grant.test"); err == nil {
			t.Fatal("an admin granted access to another org's team")
		}
		if _, err := d.svc().GrantSubmitAccess(d.ctx, d.principal(d.stranger), d.otherTeam2, "requester@grant.test"); err != nil {
			t.Fatalf("the other org's owner cannot grant on their own team: %v", err)
		}
		if g := d.grantOf(d.otherTeam2, d.customer); g == nil || g.OrgID != d.otherOrg {
			t.Fatalf("grant on the other org's team = %+v", g)
		}
	})
}

// ---- Audit ----

// TestSubmitGrantAudit: one row per grant and per revocation, with the acting
// principal, the channel it came through, the team as the target, the
// recipient's id, a readable before/after and no credential.
func TestSubmitGrantAudit(t *testing.T) {
	t.Run("grant and revocation carry the actor, target and state", func(t *testing.T) {
		d := newGrantDB(t)
		svc := d.svc()
		if _, err := svc.GrantSubmitAccess(d.ctx, d.principal(d.lead), d.team, "requester@grant.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.RevokeSubmitAccess(d.ctx, d.principal(d.admin), d.team, "requester@grant.test"); err != nil {
			t.Fatal(err)
		}
		rows := d.grantAuditRows()
		if len(rows) != 2 {
			t.Fatalf("%d audit rows, want 2 (one grant, one revocation): %+v", len(rows), rows)
		}
		set, rev := rows[0], rows[1]

		if set.action != "team.submit_grant.set" || rev.action != "team.submit_grant.revoke" {
			t.Fatalf("actions = %q, %q", set.action, rev.action)
		}
		if set.actorUserID == nil || *set.actorUserID != d.lead || set.actorEmail != "lead@grant.test" {
			t.Fatalf("grant actor = %v %q, want the team lead", set.actorUserID, set.actorEmail)
		}
		if rev.actorUserID == nil || *rev.actorUserID != d.admin || rev.actorEmail != "admin@grant.test" {
			t.Fatalf("revoke actor = %v %q, want the org owner who revoked", rev.actorUserID, rev.actorEmail)
		}
		for _, r := range rows {
			if r.actorType != domain.AuditActorHuman || r.outcome != domain.AuditOutcomeSuccess {
				t.Fatalf("%s: actor_type %q, outcome %q", r.action, r.actorType, r.outcome)
			}
			if r.targetType != "team" || r.targetID != strconv.Itoa(d.team) || r.targetName != "Inbox" {
				t.Fatalf("%s: target = %s:%s (%q)", r.action, r.targetType, r.targetID, r.targetName)
			}
			if r.orgID == nil || *r.orgID != d.org {
				t.Fatalf("%s: org = %v, want %d (an org admin must find it)", r.action, r.orgID, d.org)
			}
			if got, _ := r.details["user_id"].(float64); int(got) != d.customer {
				t.Fatalf("%s: details.user_id = %v, want the recipient %d", r.action, r.details["user_id"], d.customer)
			}
			if r.details["channel"] != auth.ChannelUI {
				t.Fatalf("%s: details.channel = %v, want %q", r.action, r.details["channel"], auth.ChannelUI)
			}
		}
		if set.before["submit_grant"] != nil || set.aftr["submit_grant"] != "in force" {
			t.Fatalf("grant before/after = %v → %v", set.before, set.aftr)
		}
		if rev.before["submit_grant"] != "in force" || rev.aftr["submit_grant"] != "revoked" {
			t.Fatalf("revoke before/after = %v → %v", rev.before, rev.aftr)
		}
		if set.details["already_active"] != false || rev.details["already_revoked"] != false {
			t.Fatalf("first-time flags = %v / %v", set.details["already_active"], rev.details["already_revoked"])
		}
	})

	t.Run("a replayed grant and a repeated revocation are recorded as the no-ops they are", func(t *testing.T) {
		d := newGrantDB(t)
		svc := d.svc()
		for i := 0; i < 2; i++ {
			if _, err := svc.GrantSubmitAccess(d.ctx, d.principal(d.lead), d.team, "requester@grant.test"); err != nil {
				t.Fatal(err)
			}
		}
		g := d.grantOf(d.team, d.customer) // attribution is still the first grantor's
		if g.GrantedBy == nil || *g.GrantedBy != d.lead {
			t.Fatalf("replayed grant changed its grantor: %+v", g)
		}
		for i := 0; i < 2; i++ {
			if _, err := svc.RevokeSubmitAccess(d.ctx, d.principal(d.lead), d.team, "requester@grant.test"); err != nil {
				t.Fatal(err)
			}
		}
		rows := d.grantAuditRows()
		if len(rows) != 4 {
			t.Fatalf("%d audit rows, want 4 (each call is recorded): %+v", len(rows), rows)
		}
		if rows[1].details["already_active"] != true {
			t.Fatalf("the replayed grant is not marked already_active: %v", rows[1].details)
		}
		if rows[3].details["already_revoked"] != true {
			t.Fatalf("the repeated revocation is not marked already_revoked: %v", rows[3].details)
		}
	})

	t.Run("no credential reaches the audit row", func(t *testing.T) {
		d := newGrantDB(t)
		// A token-authenticated MCP caller: the entries must name the token by
		// id only, never its value, and the details must be the grant's.
		p := d.principal(d.lead)
		p.Channel, p.AuthMethod, p.ViaToken, p.TokenID = auth.ChannelMCP, auth.AuthAPIToken, true, 4242
		p.Req = &reqctx.Info{Listener: reqctx.ListenerMCP, UserAgent: "claude-code/2", RequestID: "req-9"}
		if _, err := d.svc().GrantSubmitAccess(d.ctx, p, d.team, "requester@grant.test"); err != nil {
			t.Fatal(err)
		}
		rows := d.grantAuditRows()
		if len(rows) != 1 {
			t.Fatalf("%d rows: %+v", len(rows), rows)
		}
		r := rows[0]
		if r.details["channel"] != auth.ChannelMCP || r.client != domain.AuditClientMCP {
			t.Fatalf("channel/client = %v/%q, want %q/%q", r.details["channel"], r.client, auth.ChannelMCP, domain.AuditClientMCP)
		}
		// The details are the grant's, plus the context every audit row carries.
		allowed := map[string]bool{"user_id": true, "already_active": true, "channel": true, "ip_reason": true, "xff": true}
		for k, v := range r.details {
			if !allowed[k] {
				t.Fatalf("unexpected audit detail %q = %v (no token, secret or private data belongs here)", k, v)
			}
			if s, ok := v.(string); ok && strings.HasPrefix(s, "amp_") {
				t.Fatalf("audit detail %q looks like a token: %q", k, s)
			}
		}
		if tokenID := tokenIDOf(t, d, r); tokenID != nil && *tokenID != 4242 {
			t.Fatalf("token_id = %v, want the principal's 4242", *tokenID)
		}
	})

	t.Run("an agent's grant names the person it acts for", func(t *testing.T) {
		d := newGrantDB(t)
		// Put the agent on the team as a lead, in the org too (team management
		// starts from org membership): an agent with its own access is a manager
		// like any other, and the audit row must show who it acted for.
		d.exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, d.org, d.agent)
		d.exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, d.team, d.agent)
		p := d.principal(d.agent)
		p.Agent, p.ManagedBy, p.Channel, p.AuthMethod = true, &d.lead, auth.ChannelMCP, auth.AuthAPIToken
		if _, err := d.svc().GrantSubmitAccess(d.ctx, p, d.team, "requester@grant.test"); err != nil {
			t.Fatalf("an agent that leads the team cannot grant: %v", err)
		}
		rows := d.grantAuditRows()
		if len(rows) != 1 {
			t.Fatalf("%d rows: %+v", len(rows), rows)
		}
		if rows[0].actorType != domain.AuditActorAgent {
			t.Fatalf("actor_type = %q, want agent", rows[0].actorType)
		}
		if rows[0].onBehalfOf == nil || *rows[0].onBehalfOf != d.lead {
			t.Fatalf("on_behalf_of = %v, want the managing person %d", rows[0].onBehalfOf, d.lead)
		}
		if rows[0].actorUserID == nil || *rows[0].actorUserID != d.agent {
			t.Fatalf("the agent's own id is not the actor: %v", rows[0].actorUserID)
		}
	})
}

// tokenIDOf reads the row's token_id, which the shared writer fills from the
// principal (the grant's own details must not carry it).
func tokenIDOf(t *testing.T, d *grantDB, r auditRow) *int64 {
	t.Helper()
	var id *int64
	if err := d.db.QueryRow(d.ctx,
		`SELECT token_id FROM audit_log WHERE action = 'team.submit_grant.set' ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("token_id: %v", err)
	}
	return id
}

// ---- Project visibility ----

// TestSubmitGrantChangesNoProjectAccess: the grant is a capability outside the
// project level ladder. Granting it creates no membership row and reaches no
// project; revoking it takes nothing else away.
func TestSubmitGrantChangesNoProjectAccess(t *testing.T) {
	d := newGrantDB(t)
	svc := d.svc()

	// The team member reaches the project through membership; the customer does
	// not, and a grant must not change that.
	if n := d.projectAccess(d.member); n != 1 {
		t.Fatalf("the team member reaches %d projects, want 1", n)
	}
	if n := d.projectAccess(d.customer); n != 0 {
		t.Fatalf("the customer reaches %d projects before any grant, want 0", n)
	}
	orgRoles, teamRoles := d.memberships(d.customer)

	if _, err := svc.GrantSubmitAccess(d.ctx, d.principal(d.lead), d.team, "requester@grant.test"); err != nil {
		t.Fatal(err)
	}
	if n := d.projectAccess(d.customer); n != 0 {
		t.Fatalf("a granted customer reaches %d projects, want 0", n)
	}
	// The authorization layer agrees: the project reads as not found to the
	// granted customer (never 403, never a level), which is what "requester
	// access does not imply project read" means in code.
	customer := d.principal(d.customer)
	checker := authz.NewChecker(d.repo())
	if _, err := checker.RequireProject(d.ctx, customer, d.project, authz.Member); err == nil {
		t.Fatal("a granted customer reached the project's tickets")
	} else if status := authz.StatusOf(err, 500); status != 404 {
		t.Fatalf("the granted customer's project read = %d (%v), want 404", status, err)
	}
	if lvl, _ := checker.ProjectLevel(d.ctx, customer, d.project); lvl != authz.None {
		t.Fatalf("a granted customer's project level = %s, want none", lvl)
	}
	gotOrg, gotTeam := d.memberships(d.customer)
	if len(gotOrg) != len(orgRoles) || len(gotTeam) != len(teamRoles) {
		t.Fatalf("granting created membership: org %v→%v, team %v→%v", orgRoles, gotOrg, teamRoles, gotTeam)
	}

	// The project's own membership is unchanged too: no one was added to the team
	// as a side effect.
	var teamMembers, orgMembers int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM team_members WHERE team_id = $1`, d.team).Scan(&teamMembers); err != nil {
		t.Fatal(err)
	}
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM org_members WHERE org_id = $1`, d.org).Scan(&orgMembers); err != nil {
		t.Fatal(err)
	}
	if teamMembers != 2 || orgMembers != 5 {
		t.Fatalf("membership counts after a grant: %d team, %d org", teamMembers, orgMembers)
	}

	// Granting to a person who already has project access confers nothing extra.
	before := d.projectAccess(d.member)
	if _, err := svc.GrantSubmitAccess(d.ctx, d.principal(d.lead), d.team, "member@grant.test"); err != nil {
		t.Fatal(err)
	}
	if after := d.projectAccess(d.member); after != before {
		t.Fatalf("a grant changed a member's project access: %d → %d", before, after)
	}

	// Revoking takes submit access and nothing else.
	if _, err := svc.RevokeSubmitAccess(d.ctx, d.principal(d.lead), d.team, "requester@grant.test"); err != nil {
		t.Fatal(err)
	}
	if d.maySubmit(d.team, d.customer) {
		t.Fatal("submit access survived revocation")
	}
	if n := d.projectAccess(d.member); n != 1 {
		t.Fatalf("revoking a customer's grant changed a member's project access: %d", n)
	}
	if g := d.grantOf(d.team, d.customer); g == nil || g.Active() {
		t.Fatalf("the revoked grant row is not the revoked state it should be: %+v", g)
	}
}

// TestSubmitGrantListing: the administration listing shows in-force grants first
// and keeps revoked ones, because "who was taken away" is the same question.
func TestSubmitGrantListing(t *testing.T) {
	d := newGrantDB(t)
	svc := d.svc()
	if _, err := svc.GrantSubmitAccess(d.ctx, d.principal(d.lead), d.team, "requester@grant.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GrantSubmitAccess(d.ctx, d.principal(d.lead), d.team, "member2@grant.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RevokeSubmitAccess(d.ctx, d.principal(d.lead), d.team, "member2@grant.test"); err != nil {
		t.Fatal(err)
	}
	list, err := svc.ListSubmitGrants(d.ctx, d.principal(d.lead), d.team)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("listing = %+v, want both grants", list)
	}
	if list[0].UserID != d.customer || !list[0].Active() {
		t.Fatalf("the in-force grant is not first: %+v", list[0])
	}
	if list[1].UserID != d.member2 || list[1].Active() || list[1].RevokedBy == nil || *list[1].RevokedBy != d.lead {
		t.Fatalf("the revoked grant is not last or lost its revoker: %+v", list[1])
	}
	// Another team's administration sees none of it.
	other, err := svc.ListSubmitGrants(d.ctx, d.principal(d.lead2), d.otherTeam)
	if err != nil || len(other) != 0 {
		t.Fatalf("the other team's listing = %+v, %v", other, err)
	}
	// A revoked grant can be revived, attributed to whoever revived it.
	admin := d.principal(d.admin)
	if _, err := svc.GrantSubmitAccess(d.ctx, admin, d.team, "member2@grant.test"); err != nil {
		t.Fatal(err)
	}
	revived := d.grantOf(d.team, d.member2)
	if !revived.Active() || revived.GrantedBy == nil || *revived.GrantedBy != d.admin {
		t.Fatalf("revived grant = %+v (the access in force is the one this call created)", revived)
	}
	// A revocation with no grant at all is a not-found, and writes nothing.
	if _, err := svc.RevokeSubmitAccess(d.ctx, d.principal(d.lead), d.team, "noteam-grant@grant.test"); err == nil {
		t.Fatal("revoking a grant that does not exist succeeded")
	}
	var n int
	if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM audit_log WHERE action = 'team.submit_grant.revoke'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d revocation audit rows, want 1 (the refused one wrote none)", n)
	}
}
