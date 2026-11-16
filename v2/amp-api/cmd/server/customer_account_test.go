package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/service"
)

// Customer accounts (AMP #3766, approved contract artifact 36 v2, build slice
// B1): users.kind gains 'customer', and a customer is refused org owner/admin
// on every grant path. These tests prove both on a real database:
//
//   - migration 057 applies fresh and on top of 056, and its CHECK is the
//     domain's closed set (a fourth kind, or any misspelling, is refused);
//   - an existing human or agent row is untouched by the migration, and no
//     human is converted to a customer;
//   - a customer's row persists with kind 'customer';
//   - SetOrgMember, CreateOrg, AcceptInvitesList, the service's AddOrgMember
//     and Invite, and the global-admin role all refuse a customer owner/admin
//     while leaving a human's and an agent's paths exactly as they were.

// customerDB is a fresh database at the current migration level with a person,
// an agent, a customer and an org.
type customerDB struct {
	t   *testing.T
	db  *pgx.Conn
	dsn string
	ctx context.Context
	org int
	// team is a team in org, with no members but the ones a test adds.
	team int
	// person, agent and customer are users rows, of those kinds.
	person, agent, customer int
}

func newCustomerDB(t *testing.T) *customerDB {
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
	d := &customerDB{t: t, db: db, dsn: dsn, ctx: ctx}
	d.org = d.insert(`INSERT INTO orgs (name, slug) VALUES ('Customer org', 'customer-org') RETURNING id`)
	d.team = d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Inbox') RETURNING id`, d.org)
	d.person = d.insert(`INSERT INTO users (subject, email, kind) VALUES ('cust-person', 'person@customer.test', 'human') RETURNING id`)
	d.agent = d.insert(`INSERT INTO users (subject, email, kind) VALUES ('cust-agent', 'agent@customer.test', 'agent') RETURNING id`)
	d.customer = d.insert(`INSERT INTO users (subject, email, kind) VALUES ('cust-customer', 'requester@customer.test', 'customer') RETURNING id`)
	return d
}

func (d *customerDB) insert(sql string, args ...any) int {
	d.t.Helper()
	var id int
	if err := d.db.QueryRow(d.ctx, sql, args...).Scan(&id); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (d *customerDB) kindOf(userID int) string {
	d.t.Helper()
	var kind string
	if err := d.db.QueryRow(d.ctx, `SELECT kind FROM users WHERE id = $1`, userID).Scan(&kind); err != nil {
		d.t.Fatalf("kind of %d: %v", userID, err)
	}
	return kind
}

// orgRole returns the user's role in the fixture org, "" when there is no row.
func (d *customerDB) orgRole(userID int) string {
	d.t.Helper()
	var role string
	err := d.db.QueryRow(d.ctx, `SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, d.org, userID).Scan(&role)
	if err == pgx.ErrNoRows {
		return ""
	}
	if err != nil {
		d.t.Fatalf("org role of %d: %v", userID, err)
	}
	return role
}

// teamRole returns the user's role on the fixture team, "" when there is no
// row.
func (d *customerDB) teamRole(userID int) string {
	d.t.Helper()
	var role string
	err := d.db.QueryRow(d.ctx, `SELECT role FROM team_members WHERE team_id = $1 AND user_id = $2`, d.team, userID).Scan(&role)
	if err == pgx.ErrNoRows {
		return ""
	}
	if err != nil {
		d.t.Fatalf("team role of %d: %v", userID, err)
	}
	return role
}

// auditActions returns the audit_log actions recorded against the fixture
// team, newest first.
func (d *customerDB) auditActions() []string {
	d.t.Helper()
	rows, err := d.db.Query(d.ctx, `SELECT action FROM audit_log WHERE target_type = 'team' AND target_id = $1 ORDER BY id DESC`, fmt.Sprint(d.team))
	if err != nil {
		d.t.Fatalf("audit actions: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			d.t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func (d *customerDB) repo() *repository.Repo {
	d.t.Helper()
	repo, err := repository.New(d.ctx, d.dsn)
	if err != nil {
		d.t.Fatalf("connect: %v", err)
	}
	d.t.Cleanup(repo.Close)
	return repo
}

// svc builds the org service over the fixture database; the audit writer and
// the remote-session ends it calls are nil-safe here (no registry, no relay).
func (d *customerDB) svc() (*service.Service, *authz.Checker) {
	d.t.Helper()
	repo := d.repo()
	return service.New(repo, nil, nil), authz.NewChecker(repo)
}

// principal is a signed-in caller for service methods.
func (d *customerDB) principal(userID int) authz.Principal {
	var email string
	if err := d.db.QueryRow(d.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		d.t.Fatalf("email of %d: %v", userID, err)
	}
	return authz.Principal{UserID: userID, Email: email, Channel: auth.ChannelUI}
}

// ---- Migration ----

func TestCustomerMigration057(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh database accepts customer and refuses other kinds", func(t *testing.T) {
		d := newCustomerDB(t)
		if got := d.kindOf(d.customer); got != domain.UserKindCustomer {
			t.Fatalf("customer row kind = %q", got)
		}
		for i, bad := range []string{"client", "Customer", "", "customer "} {
			_, err := d.db.Exec(ctx, `INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3)`,
				fmt.Sprintf("bad-%d", i), fmt.Sprintf("bad-%d@customer.test", i), bad)
			if err == nil {
				t.Fatalf("kind %q was accepted; the CHECK must be the domain's closed set", bad)
			}
		}
	})

	t.Run("upgrade from 056 keeps existing humans and agents", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		var before []repository.Migration
		for _, m := range migrations {
			if m.Version < "057_customer_accounts" {
				before = append(before, m)
			}
		}
		if len(before) == len(migrations) {
			t.Fatal("no migration 057")
		}
		if err := repo.RunMigrations(ctx, before); err != nil {
			t.Fatalf("migrate to 056: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		// Work that exists at 056: a person, an agent, an org with an owner.
		var person, agent, org int
		if err := db.QueryRow(ctx, `INSERT INTO users (subject, email) VALUES ('pre-person', 'pre@customer.test') RETURNING id`).Scan(&person); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `INSERT INTO users (subject, email, kind) VALUES ('pre-agent', 'pre-agent@agents.amp', 'agent') RETURNING id`).Scan(&agent); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `INSERT INTO orgs (name, slug) VALUES ('Pre org', 'pre-org') RETURNING id`).Scan(&org); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, org, person); err != nil {
			t.Fatal(err)
		}

		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("upgrade to 057: %v", err)
		}
		var personKind, agentKind, role string
		if err := db.QueryRow(ctx, `SELECT kind FROM users WHERE id = $1`, person).Scan(&personKind); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `SELECT kind FROM users WHERE id = $1`, agent).Scan(&agentKind); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, org, person).Scan(&role); err != nil {
			t.Fatal(err)
		}
		if personKind != domain.UserKindHuman || agentKind != domain.UserKindAgent {
			t.Fatalf("upgrade changed account kinds: person %q, agent %q", personKind, agentKind)
		}
		if role != domain.OrgRoleOwner {
			t.Fatalf("upgrade changed an existing owner membership: %q", role)
		}
		// Re-running 057 by hand changes nothing — one CHECK, not two.
		var sql string
		for _, m := range migrations {
			if m.Version == "057_customer_accounts" {
				sql = m.SQL
			}
		}
		if sql == "" {
			t.Fatal("migration 057 not embedded")
		}
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatalf("re-running 057: %v", err)
		}
		var constraints int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = 'users_kind_check' AND conrelid = 'users'::regclass`).Scan(&constraints); err != nil {
			t.Fatal(err)
		}
		if constraints != 1 {
			t.Fatalf("users_kind_check exists %d times, want 1", constraints)
		}
	})

	t.Run("customer kind persists through the repository", func(t *testing.T) {
		d := newCustomerDB(t)
		got, err := d.repo().GetUserByID(ctx, d.customer)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != domain.UserKindCustomer || !got.IsCustomer() || got.IsAgent() {
			t.Fatalf("stored customer = %+v", got)
		}
	})
}

// ---- The administration invariant on the repository grant paths ----

func TestCustomerOrgRoleRefusedInRepository(t *testing.T) {
	ctx := context.Background()

	t.Run("SetOrgMember refuses owner and admin, allows member", func(t *testing.T) {
		d := newCustomerDB(t)
		repo := d.repo()
		for _, role := range []string{domain.OrgRoleOwner, domain.OrgRoleAdmin} {
			err := repo.SetOrgMember(ctx, d.org, d.customer, role)
			if err == nil {
				t.Fatalf("customer granted org %s", role)
			}
			if !strings.Contains(err.Error(), "customer") {
				t.Fatalf("refusal does not name the customer account: %v", err)
			}
			if got := d.orgRole(d.customer); got != "" {
				t.Fatalf("refused %s grant still wrote role %q", role, got)
			}
		}
		if err := repo.SetOrgMember(ctx, d.org, d.customer, domain.OrgRoleMember); err != nil {
			t.Fatalf("customer may be a plain member: %v", err)
		}
		if got := d.orgRole(d.customer); got != domain.OrgRoleMember {
			t.Fatalf("customer role = %q", got)
		}
		// Promoting a member customer afterwards is refused too.
		if err := repo.SetOrgMember(ctx, d.org, d.customer, domain.OrgRoleAdmin); err == nil {
			t.Fatal("customer member promoted to admin")
		}
	})

	t.Run("existing people and agents are unaffected", func(t *testing.T) {
		d := newCustomerDB(t)
		repo := d.repo()
		for _, u := range []int{d.person, d.agent} {
			// Admin first, then owner: demoting the org's only owner is
			// refused by the pre-existing last-owner guard, which is not
			// what this test is about.
			if err := repo.SetOrgMember(ctx, d.org, u, domain.OrgRoleAdmin); err != nil {
				t.Fatalf("user %d refused admin: %v", u, err)
			}
			if got := d.orgRole(u); got != domain.OrgRoleAdmin {
				t.Fatalf("user %d role = %q", u, got)
			}
			if err := repo.SetOrgMember(ctx, d.org, u, domain.OrgRoleOwner); err != nil {
				t.Fatalf("user %d refused owner: %v", u, err)
			}
			if got := d.orgRole(u); got != domain.OrgRoleOwner {
				t.Fatalf("user %d role = %q", u, got)
			}
		}
	})

	t.Run("CreateOrg refuses a customer as its first owner", func(t *testing.T) {
		d := newCustomerDB(t)
		repo := d.repo()
		if _, err := repo.CreateOrg(ctx, "Refused org", "refused-org", nil, &d.customer); err == nil {
			t.Fatal("org created with a customer owner")
		}
		var orgs int
		if err := d.db.QueryRow(ctx, `SELECT count(*) FROM orgs WHERE slug = 'refused-org'`).Scan(&orgs); err != nil {
			t.Fatal(err)
		}
		if orgs != 0 {
			t.Fatalf("refused CreateOrg still created %d org row(s)", orgs)
		}
		// A person as the first owner is unchanged.
		org, err := repo.CreateOrg(ctx, "Fine org", "fine-org", nil, &d.person)
		if err != nil {
			t.Fatalf("person as first owner: %v", err)
		}
		role, err := repo.GetOrgRole(ctx, org.ID, d.person)
		if err != nil || role != domain.OrgRoleOwner {
			t.Fatalf("person role in new org = %q %v", role, err)
		}
	})

	t.Run("AcceptInvitesList refuses an owner invite to a customer", func(t *testing.T) {
		d := newCustomerDB(t)
		repo := d.repo()
		// An admin invites an email that has no account yet as an owner; the
		// person then signs in as a customer and the invite is accepted.
		inv, err := repo.CreateInvite(ctx, domain.Invite{
			Email: "later@customer.test", OrgID: d.org, OrgRole: domain.OrgRoleOwner, TeamRole: domain.TeamRoleMember,
		})
		if err != nil {
			t.Fatal(err)
		}
		var later int
		if err := d.db.QueryRow(ctx, `INSERT INTO users (subject, email, kind) VALUES ('cust-later', 'later@customer.test', 'customer') RETURNING id`).Scan(&later); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.AcceptInvitesList(ctx, later, "later@customer.test"); err == nil {
			t.Fatal("a customer accepted an org owner invite")
		}
		if got := d.orgRole(later); got != "" {
			t.Fatalf("refused invite still wrote role %q", got)
		}
		// The invite stays pending, so an admin can revoke or re-issue it.
		pending, err := repo.ListPendingInvites(ctx, d.org)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range pending {
			if p.ID == inv.ID {
				found = true
			}
		}
		if !found {
			t.Fatal("the refused invite was consumed; it must stay pending")
		}
		// A human accepting a plain owner invite is unchanged.
		hInv, err := repo.CreateInvite(ctx, domain.Invite{
			Email: "human-later@customer.test", OrgID: d.org, OrgRole: domain.OrgRoleOwner, TeamRole: domain.TeamRoleMember,
		})
		if err != nil {
			t.Fatal(err)
		}
		var human int
		if err := d.db.QueryRow(ctx, `INSERT INTO users (subject, email, kind) VALUES ('human-later', 'human-later@customer.test', 'human') RETURNING id`).Scan(&human); err != nil {
			t.Fatal(err)
		}
		accepted, err := repo.AcceptInvitesList(ctx, human, "human-later@customer.test")
		if err != nil {
			t.Fatalf("a person accepting an owner invite: %v", err)
		}
		if len(accepted) != 1 || accepted[0].ID != hInv.ID {
			t.Fatalf("a person's owner invite was not accepted: %+v", accepted)
		}
		if got := d.orgRole(human); got != domain.OrgRoleOwner {
			t.Fatalf("person's accepted role = %q", got)
		}
	})
}

// ---- The administration invariant on the service grant paths ----

func TestCustomerOrgRoleRefusedInService(t *testing.T) {
	ctx := context.Background()

	t.Run("AddOrgMember refuses a customer owner or admin", func(t *testing.T) {
		d := newCustomerDB(t)
		svc, _ := d.svc()
		// The caller is an org owner, so the refusal is about the target's
		// kind, not the caller's rights.
		if err := d.repo().SetOrgMember(ctx, d.org, d.person, domain.OrgRoleOwner); err != nil {
			t.Fatal(err)
		}
		admin := d.principal(d.person)
		for _, role := range []string{domain.OrgRoleOwner, domain.OrgRoleAdmin} {
			_, err := svc.AddOrgMember(ctx, admin, d.org, fmt.Sprint(d.customer), role, false)
			if err == nil {
				t.Fatalf("service granted a customer org %s", role)
			}
			if !strings.Contains(err.Error(), "customer") {
				t.Fatalf("refusal does not name the customer account: %v", err)
			}
			if got := d.orgRole(d.customer); got != "" {
				t.Fatalf("refused %s grant wrote role %q", role, got)
			}
		}
		if _, err := svc.AddOrgMember(ctx, admin, d.org, fmt.Sprint(d.customer), domain.OrgRoleMember, false); err != nil {
			t.Fatalf("a customer may be a plain org member: %v", err)
		}
		if got := d.orgRole(d.customer); got != domain.OrgRoleMember {
			t.Fatalf("customer role = %q", got)
		}
	})

	t.Run("Invite refuses an owner invitation to an existing customer", func(t *testing.T) {
		d := newCustomerDB(t)
		svc, _ := d.svc()
		if err := d.repo().SetOrgMember(ctx, d.org, d.person, domain.OrgRoleOwner); err != nil {
			t.Fatal(err)
		}
		admin := d.principal(d.person)
		_, err := svc.Invite(ctx, admin, domain.Invite{
			Email: "requester@customer.test", OrgID: d.org, OrgRole: domain.OrgRoleOwner, TeamRole: domain.TeamRoleMember,
		})
		if err == nil {
			t.Fatal("an existing customer was invited as an org owner")
		}
		if !strings.Contains(err.Error(), "customer") {
			t.Fatalf("refusal does not name the customer account: %v", err)
		}
		if got := d.orgRole(d.customer); got != "" {
			t.Fatalf("refused invite wrote role %q", got)
		}
		// An unknown email is invited as before: nothing to check until it has
		// an account, and acceptance re-checks the kind.
		res, err := svc.Invite(ctx, admin, domain.Invite{
			Email: "brand-new@customer.test", OrgID: d.org, OrgRole: domain.OrgRoleMember, TeamRole: domain.TeamRoleMember,
		})
		if err != nil || !res.Invited {
			t.Fatalf("inviting an unknown email: %+v %v", res, err)
		}
	})

	t.Run("a customer can't be a global admin", func(t *testing.T) {
		d := newCustomerDB(t)
		svc, _ := d.svc()
		root := d.principal(d.person)
		root.Super = true
		err := svc.SetUserRole(ctx, root, d.customer, domain.RoleAdmin, true)
		if err == nil {
			t.Fatal("a customer was made a global admin")
		}
		if !strings.Contains(err.Error(), "customer") {
			t.Fatalf("refusal does not name the customer account: %v", err)
		}
		var admins int
		if err := d.db.QueryRow(ctx, `SELECT count(*) FROM user_roles WHERE user_id = $1 AND role = 'admin'`, d.customer).Scan(&admins); err != nil {
			t.Fatal(err)
		}
		if admins != 0 {
			t.Fatal("refused grant still wrote a global admin role")
		}
		// The member role is not an administration role and stays allowed.
		if err := svc.SetUserRole(ctx, root, d.customer, domain.RoleMember, true); err != nil {
			t.Fatalf("granting the member role to a customer: %v", err)
		}
	})

	t.Run("whoami reports the account kind", func(t *testing.T) {
		w := newRBACWorld(t)
		ctx := context.Background()
		// A customer row in the stack's own database. No production path mints
		// one yet (invite provisioning is a separate ticket), so the fixture
		// writes the kind straight in — exactly what a provisioned customer
		// row looks like.
		sfx := fmt.Sprintf("%d", time.Now().UnixNano())
		row, err := w.s.repo.UpsertUserFromClaims(ctx, "sub-customer-"+sfx, "customer-"+sfx+"@rbac.test", "customer", nil)
		if err != nil {
			t.Fatalf("provision customer: %v", err)
		}
		db, err := pgx.Connect(ctx, testDSN(t))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		if _, err := db.Exec(ctx, `UPDATE users SET kind = 'customer' WHERE id = $1`, row.ID); err != nil {
			t.Fatalf("make it a customer: %v", err)
		}
		row.Kind = domain.UserKindCustomer
		ident := &auth.Identity{Subject: row.Subject, Email: row.Email, Name: row.DisplayName, User: row}
		res := w.as(ident).call(t, "amp_whoami", map[string]interface{}{})
		if got := fmt.Sprint(res["account"]); got != domain.UserKindCustomer {
			t.Fatalf("whoami account = %q", got)
		}
		if res["global_admin"] != false {
			t.Fatalf("a customer reported as a global admin: %v", res["global_admin"])
		}
		// The same tool still names a person and an agent as before.
		if got := fmt.Sprint(w.as(w.alice).call(t, "amp_whoami", map[string]interface{}{})["account"]); got != domain.UserKindHuman {
			t.Fatalf("whoami account for %s = %q, want human", w.alice.Email, got)
		}
		bot, err := w.s.repo.CreateAgentUser(ctx, "agent:whoami-"+sfx, "bot-"+sfx+"@agents.amp", "bot", &w.alice.User.ID)
		if err != nil {
			t.Fatalf("agent: %v", err)
		}
		botIdent := &auth.Identity{Subject: bot.Subject, Email: bot.Email, Name: "bot", User: bot}
		if got := fmt.Sprint(w.as(botIdent).call(t, "amp_whoami", map[string]interface{}{})["account"]); got != domain.UserKindAgent {
			t.Fatalf("agent whoami account = %q", got)
		}
	})
}

// ---- Team membership: the other half of the requester-isolation invariant ----

// TestCustomerTeamMembershipRefused (AMP #3861) closes the gap the wave review
// of #3766-#3772 found: the invite-acceptance path refused team membership for
// a customer, but the *direct* add path (`Service.AddTeamMember` →
// `Repo.SetTeamMember`) did not. Team membership is how a person reaches every
// project a team is linked to (`AccessibleProjectIDs`), so it is the one
// membership a requester identity may never hold.
//
// Proved here on a real database, at both layers:
//   - the service refuses a customer add and writes no team_members row, no
//     org_members row, and no successful-add audit entry;
//   - the repository refuses it as a backstop when SetTeamMember is called
//     directly with a customer id, inside its writing transaction;
//   - a human's direct add is unchanged, and so is CreateAgent's grant to the
//     freshly created agent row it manages;
//   - plain org membership for a customer is still allowed (deliberate).
func TestCustomerTeamMembershipRefused(t *testing.T) {
	ctx := context.Background()

	t.Run("Service.AddTeamMember refuses a customer", func(t *testing.T) {
		d := newCustomerDB(t)
		svc, _ := d.svc()
		// The caller is an org owner, so the refusal is about the target's
		// kind, not the caller's rights (RequireTeamManager passes).
		if err := d.repo().SetOrgMember(ctx, d.org, d.person, domain.OrgRoleOwner); err != nil {
			t.Fatal(err)
		}
		lead := d.principal(d.person)
		// Both ways of naming the customer: by email, and by id.
		for _, ref := range []string{"requester@customer.test", fmt.Sprint(d.customer)} {
			res, err := svc.AddTeamMember(ctx, lead, d.team, ref, domain.TeamRoleMember, false)
			if err == nil {
				t.Fatalf("service added customer %q to the team: %+v", ref, res)
			}
			if !strings.Contains(err.Error(), "customer") {
				t.Fatalf("refusal (%q) does not name the customer account: %v", ref, err)
			}
			// Same conflict as the invite-acceptance path (#3861): 409.
			if got := authz.StatusOf(err, 500); got != 409 {
				t.Fatalf("refusal (%q) status = %d, want 409", ref, got)
			}
			// A customer that did not join the team has no routing grant
			// either: the refusal suggests one, it does not silently add it.
			if got := d.teamRole(d.customer); got != "" {
				t.Fatalf("refused add (%q) wrote team role %q", ref, got)
			}
			if got := d.orgRole(d.customer); got != "" {
				t.Fatalf("refused add (%q) wrote org role %q", ref, got)
			}
		}
		for _, a := range d.auditActions() {
			if a == "team.member.set" {
				t.Fatalf("refused add still audited a successful grant (%v)", d.auditActions())
			}
		}
	})

	t.Run("Service.AddTeamMember still adds a human", func(t *testing.T) {
		d := newCustomerDB(t)
		svc, _ := d.svc()
		if err := d.repo().SetOrgMember(ctx, d.org, d.person, domain.OrgRoleOwner); err != nil {
			t.Fatal(err)
		}
		lead := d.principal(d.person)
		// A human, directly by email: the regression the ticket asks for.
		res, err := svc.AddTeamMember(ctx, lead, d.team, "person@customer.test", domain.TeamRoleMember, false)
		if err != nil {
			t.Fatalf("a person's direct add was refused: %v", err)
		}
		if res == nil || !res.Added || res.UserID != d.person || res.Role != domain.TeamRoleMember {
			t.Fatalf("person add = %+v", res)
		}
		if got := d.teamRole(d.person); got != domain.TeamRoleMember {
			t.Fatalf("person team role = %q", got)
		}
		if got := d.orgRole(d.person); got == "" {
			t.Fatal("a direct team add no longer gives the person org membership")
		}
		// Promoting them to lead on the same team is unchanged too.
		if _, err := svc.AddTeamMember(ctx, lead, d.team, fmt.Sprint(d.person), domain.TeamRoleLead, false); err != nil {
			t.Fatalf("promoting a person to team lead: %v", err)
		}
		if got := d.teamRole(d.person); got != domain.TeamRoleLead {
			t.Fatalf("person team role after promotion = %q", got)
		}
		// The successful adds are audited, so the pair of subtests shows the
		// refusal also suppresses the audit row rather than merely the insert.
		found := false
		for _, a := range d.auditActions() {
			if a == "team.member.set" {
				found = true
			}
		}
		if !found {
			t.Fatalf("a person's add is not audited: %v", d.auditActions())
		}
	})

	t.Run("Repo.SetTeamMember refuses a customer directly", func(t *testing.T) {
		d := newCustomerDB(t)
		repo := d.repo()
		// Called straight at the repository, with no service check in front of
		// it: the in-transaction guard is the backstop. Both the member and the
		// lead role are refused — team membership itself is the vector.
		for _, role := range []string{domain.TeamRoleMember, domain.TeamRoleLead} {
			err := repo.SetTeamMember(ctx, d.team, d.customer, role)
			if err == nil {
				t.Fatalf("repository granted a customer team %s", role)
			}
			if !strings.Contains(err.Error(), "customer") {
				t.Fatalf("refusal does not name the customer account: %v", err)
			}
			if !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("refusal is not a conflict (409): %v", err)
			}
			if got := d.teamRole(d.customer); got != "" {
				t.Fatalf("refused %s grant wrote team role %q", role, got)
			}
			if got := d.orgRole(d.customer); got != "" {
				t.Fatalf("refused %s grant wrote org role %q", role, got)
			}
		}
		// A customer who is already a plain org member (deliberate, and
		// allowed) still gets no team row: the guard runs before both inserts.
		if err := repo.SetOrgMember(ctx, d.org, d.customer, domain.OrgRoleMember); err != nil {
			t.Fatalf("a customer may be a plain org member: %v", err)
		}
		if err := repo.SetTeamMember(ctx, d.team, d.customer, domain.TeamRoleMember); err == nil {
			t.Fatal("repository added an org-member customer to the team")
		}
		if got := d.teamRole(d.customer); got != "" {
			t.Fatalf("refused add wrote team role %q", got)
		}
	})

	t.Run("Repo.SetTeamMember still adds a human and an agent", func(t *testing.T) {
		d := newCustomerDB(t)
		repo := d.repo()
		if err := repo.SetTeamMember(ctx, d.team, d.person, domain.TeamRoleMember); err != nil {
			t.Fatalf("person: %v", err)
		}
		if err := repo.SetTeamMember(ctx, d.team, d.agent, domain.TeamRoleMember); err != nil {
			t.Fatalf("agent: %v", err)
		}
		if got := d.teamRole(d.person); got != domain.TeamRoleMember {
			t.Fatalf("person team role = %q", got)
		}
		if got := d.teamRole(d.agent); got != domain.TeamRoleMember {
			t.Fatalf("agent team role = %q", got)
		}
	})

	t.Run("CreateAgent still puts its new agent on a team", func(t *testing.T) {
		d := newCustomerDB(t)
		svc, _ := d.svc()
		// CreateAgent calls Repo.SetTeamMember for the agent row it has just
		// created (kind='agent'), so the repository guard must not touch it.
		if err := d.repo().SetTeamMember(ctx, d.team, d.person, domain.TeamRoleLead); err != nil {
			t.Fatal(err)
		}
		owner := d.principal(d.person)
		bot, err := svc.CreateAgent(ctx, owner, "team bot", []int{d.team})
		if err != nil {
			t.Fatalf("CreateAgent on a team: %v", err)
		}
		if got := d.kindOf(bot.ID); got != domain.UserKindAgent {
			t.Fatalf("new account kind = %q", got)
		}
		if got := d.teamRole(bot.ID); got != domain.TeamRoleMember {
			t.Fatalf("agent team role = %q", got)
		}
	})
}
