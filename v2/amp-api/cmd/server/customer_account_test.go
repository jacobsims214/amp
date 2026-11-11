package main

import (
	"context"
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
