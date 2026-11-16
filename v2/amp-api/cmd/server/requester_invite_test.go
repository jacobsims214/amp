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

// Requester invitations (AMP #3767, approved contract artifact 36 v2 §5, build
// slice B1's invite half). These tests prove the boundary on a real database:
//
//   - an explicit requester invitation provisions a *customer* identity at
//     first sign-in, with no org membership, no team membership and therefore
//     no project access;
//   - an existing person, agent or team member keeps its kind and every
//     membership it had — an invitation never converts or demotes;
//   - only a team manager (team lead or org admin) can issue one, and it is
//     bound to one org, one team and one recipient;
//   - replaying the invitation replaces the pending intent instead of adding
//     a second pending row, and a revoked invitation can never be accepted;
//   - a foreign (wrong-org / wrong-team) invitation is refused.
//
// The invite's recorded kind comes from migration 058's `invites_kind_check`;
// the customer kind itself is #3766's `users_kind_check`.

// requesterDB is a fresh database at the current migration level with an org,
// a team, a team lead, an outsider and the org's admin.
type requesterDB struct {
	t    *testing.T
	db   *pgx.Conn
	dsn  string
	ctx  context.Context
	org  int
	team int
	// admin is an org owner (a team manager, like a team lead); lead is the
	// team's lead; outsider is a plain org member with no team role.
	admin, lead, outsider int
}

func newRequesterDB(t *testing.T) *requesterDB {
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
	d := &requesterDB{t: t, db: db, dsn: dsn, ctx: ctx}
	d.org = d.insert(`INSERT INTO orgs (name, slug) VALUES ('Requester org', 'requester-org') RETURNING id`)
	d.team = d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Inbox') RETURNING id`, d.org)
	d.admin = d.insert(`INSERT INTO users (subject, email, kind) VALUES ('rq-admin', 'admin@requester.test', 'human') RETURNING id`)
	d.lead = d.insert(`INSERT INTO users (subject, email, kind) VALUES ('rq-lead', 'lead@requester.test', 'human') RETURNING id`)
	d.outsider = d.insert(`INSERT INTO users (subject, email, kind) VALUES ('rq-outsider', 'outsider@requester.test', 'human') RETURNING id`)
	d.exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, d.org, d.admin)
	d.exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, d.org, d.lead)
	d.exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, d.org, d.outsider)
	d.exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'lead')`, d.team, d.lead)
	return d
}

func (d *requesterDB) insert(sql string, args ...any) int {
	d.t.Helper()
	var id int
	if err := d.db.QueryRow(d.ctx, sql, args...).Scan(&id); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (d *requesterDB) exec(sql string, args ...any) {
	d.t.Helper()
	if _, err := d.db.Exec(d.ctx, sql, args...); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
}

func (d *requesterDB) repo() *repository.Repo {
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
func (d *requesterDB) svc() *service.Service {
	d.t.Helper()
	return service.New(d.repo(), nil, nil)
}

// kindOf returns the users.kind of a row.
func (d *requesterDB) kindOf(userID int) string {
	d.t.Helper()
	var kind string
	if err := d.db.QueryRow(d.ctx, `SELECT kind FROM users WHERE id = $1`, userID).Scan(&kind); err != nil {
		d.t.Fatalf("kind of %d: %v", userID, err)
	}
	return kind
}

// principal is a signed-in caller for service methods.
func (d *requesterDB) principal(userID int) authz.Principal {
	d.t.Helper()
	var email string
	if err := d.db.QueryRow(d.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		d.t.Fatalf("email of %d: %v", userID, err)
	}
	return authz.Principal{UserID: userID, Email: email, Channel: auth.ChannelUI}
}

// memberships returns the user's org roles and team roles, as sorted strings.
func (d *requesterDB) memberships(userID int) (orgRoles, teamRoles []string) {
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

// projectAccess counts the projects the user can see, through the same query
// the authorization layer uses.
func (d *requesterDB) projectAccess(userID int) int {
	d.t.Helper()
	ids, err := d.repo().AccessibleProjectIDs(d.ctx, userID)
	if err != nil {
		d.t.Fatalf("accessible projects of %d: %v", userID, err)
	}
	return len(ids)
}

// signIn runs the real JIT provisioning path (what an OIDC sign-in does) and
// returns the provisioned user.
func (d *requesterDB) signIn(subject, email, displayName string) (*domain.User, *repository.Provisioned) {
	d.t.Helper()
	u, pv, err := d.repo().ProvisionUserFromClaims(d.ctx, subject, email, displayName, nil)
	if err != nil {
		d.t.Fatalf("provision %s: %v", email, err)
	}
	return u, pv
}

// userIDByEmail looks a user up by email, failing when there is none.
func (d *requesterDB) userIDByEmail(email string) int {
	d.t.Helper()
	var id int
	if err := d.db.QueryRow(d.ctx, `SELECT id FROM users WHERE lower(email) = lower($1)`, email).Scan(&id); err != nil {
		d.t.Fatalf("user %s: %v", email, err)
	}
	return id
}

// pendingInvites counts the pending invitations for an email.
func (d *requesterDB) pendingInvites(email string) int {
	d.t.Helper()
	var n int
	if err := d.db.QueryRow(d.ctx,
		`SELECT count(*) FROM invites WHERE lower(email) = lower($1) AND accepted_at IS NULL`, email).Scan(&n); err != nil {
		d.t.Fatal(err)
	}
	return n
}

// ---- Migration ----

func TestRequesterInviteMigration058(t *testing.T) {
	ctx := context.Background()

	t.Run("existing invites stay membership invites and an unknown kind is refused", func(t *testing.T) {
		d := newRequesterDB(t)
		// A row written the pre-#3767 way (no kind) takes the default.
		id := d.insert(`INSERT INTO invites (email, org_id, team_id) VALUES ('legacy@requester.test', $1, $2) RETURNING id`, d.org, d.team)
		var kind string
		if err := d.db.QueryRow(ctx, `SELECT kind FROM invites WHERE id = $1`, id).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if kind != domain.InviteKindMember {
			t.Fatalf("a pre-existing invite kind = %q, want %q", kind, domain.InviteKindMember)
		}
		for i, bad := range []string{"customer", "Requester", "", " owner"} {
			if _, err := d.db.Exec(ctx,
				`INSERT INTO invites (email, org_id, team_id, kind) VALUES ($1, $2, $3, $4)`,
				fmt.Sprintf("bad-%d@requester.test", i), d.org, d.team, bad); err == nil {
				t.Fatalf("invite kind %q was accepted; the CHECK must be the domain's closed set", bad)
			}
		}
	})

	t.Run("upgrade from 057 keeps existing invites and re-running changes nothing", func(t *testing.T) {
		dsn := freshDatabase(t)
		repo, err := repository.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		var before []repository.Migration
		for _, m := range migrations {
			if m.Version < "058_requester_invites" {
				before = append(before, m)
			}
		}
		if len(before) == len(migrations) {
			t.Fatal("no migration 058")
		}
		if err := repo.RunMigrations(ctx, before); err != nil {
			t.Fatalf("migrate to 057: %v", err)
		}
		db, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close(ctx)
		// Work that exists at 057: an org with a pending membership invite.
		var org, team, inv int
		if err := db.QueryRow(ctx, `INSERT INTO orgs (name, slug) VALUES ('Pre org', 'pre-org') RETURNING id`).Scan(&org); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `INSERT INTO teams (org_id, name) VALUES ($1, 'Pre team') RETURNING id`, org).Scan(&team); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `INSERT INTO invites (email, org_id, team_id) VALUES ('pre@requester.test', $1, $2) RETURNING id`, org, team).Scan(&inv); err != nil {
			t.Fatal(err)
		}

		if err := repo.RunMigrations(ctx, migrations); err != nil {
			t.Fatalf("upgrade to 058: %v", err)
		}
		var kind string
		if err := db.QueryRow(ctx, `SELECT kind FROM invites WHERE id = $1`, inv).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if kind != domain.InviteKindMember {
			t.Fatalf("upgrade changed an existing invite to %q", kind)
		}
		// Re-running 058 by hand changes nothing — one CHECK, not two.
		var sql string
		for _, m := range migrations {
			if m.Version == "058_requester_invites" {
				sql = m.SQL
			}
		}
		if sql == "" {
			t.Fatal("migration 058 not embedded")
		}
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatalf("re-running 058: %v", err)
		}
		var constraints int
		if err := db.QueryRow(ctx,
			`SELECT count(*) FROM pg_constraint WHERE conname = 'invites_kind_check' AND conrelid = 'invites'::regclass`).Scan(&constraints); err != nil {
			t.Fatal(err)
		}
		if constraints != 1 {
			t.Fatalf("invites_kind_check exists %d times, want 1", constraints)
		}
	})
}

// ---- Provisioning: the acceptance criteria ----

func TestRequesterInviteProvisionsCustomer(t *testing.T) {
	t.Run("a new identity becomes a customer with no org, team or project access", func(t *testing.T) {
		d := newRequesterDB(t)
		svc := d.svc()
		// A team lead issues the requester invitation for a brand-new email.
		teamID := d.team
		res, err := svc.Invite(d.ctx, d.principal(d.lead), domain.Invite{
			Email: "requester@new.test", OrgID: d.org, TeamID: &teamID, Kind: domain.InviteKindRequester,
		})
		if err != nil {
			t.Fatalf("team lead issuing a requester invite: %v", err)
		}
		if !res.Invited || res.Invite == nil || res.Invite.Kind != domain.InviteKindRequester {
			t.Fatalf("result = %+v, want a pending requester invite", res)
		}

		// The person signs in for the first time.
		u, pv := d.signIn("sub-requester", "requester@new.test", "Requester")
		if u.Kind != domain.UserKindCustomer {
			t.Fatalf("first sign-in kind = %q, want customer", u.Kind)
		}
		if !u.IsCustomer() || u.IsAgent() {
			t.Fatalf("customer predicates disagree: %+v", u)
		}
		// The invite was applied.
		if len(pv.Invites) != 1 || pv.Invites[0].Kind != domain.InviteKindRequester {
			t.Fatalf("accepted invites = %+v, want the requester invite", pv.Invites)
		}
		if d.pendingInvites("requester@new.test") != 0 {
			t.Fatal("the requester invite is still pending after sign-in")
		}
		// ...and granted nothing: no org role, no team role, no project access.
		orgRoles, teamRoles := d.memberships(u.ID)
		if len(orgRoles) != 0 || len(teamRoles) != 0 {
			t.Fatalf("accepting a requester invitation granted memberships: org %v, team %v", orgRoles, teamRoles)
		}
		if n := d.projectAccess(u.ID); n != 0 {
			t.Fatalf("a customer reaches %d projects, want 0", n)
		}
		// The identity is a real one: it is not an orphan row without a role.
		var globalRoles int
		if err := d.db.QueryRow(d.ctx, `SELECT count(*) FROM user_roles WHERE user_id = $1`, u.ID).Scan(&globalRoles); err != nil {
			t.Fatal(err)
		}
		if globalRoles == 0 {
			t.Fatal("a provisioned customer has no global role; it must still be a real AMP account")
		}
	})

	t.Run("a first sign-in with no requester invite stays a person", func(t *testing.T) {
		d := newRequesterDB(t)
		u, _ := d.signIn("sub-bystander", "bystander@new.test", "Bystander")
		if u.Kind != domain.UserKindHuman {
			t.Fatalf("kind = %q, want human", u.Kind)
		}
	})

	t.Run("a membership invite still provisions a person with org and team membership", func(t *testing.T) {
		d := newRequesterDB(t)
		repo := d.repo()
		teamID := d.team
		if _, err := repo.CreateInvite(d.ctx, domain.Invite{
			Email: "member@new.test", OrgID: d.org, OrgRole: domain.OrgRoleMember, TeamID: &teamID, TeamRole: domain.TeamRoleMember,
		}); err != nil {
			t.Fatal(err)
		}
		u, pv := d.signIn("sub-member", "member@new.test", "Member")
		if u.Kind != domain.UserKindHuman {
			t.Fatalf("a membership invite provisioned kind %q, want human", u.Kind)
		}
		if len(pv.Invites) != 1 {
			t.Fatalf("accepted invites = %+v", pv.Invites)
		}
		orgRoles, teamRoles := d.memberships(u.ID)
		if len(orgRoles) != 1 || orgRoles[0] != domain.OrgRoleMember {
			t.Fatalf("org roles = %v, want one member", orgRoles)
		}
		if len(teamRoles) != 1 || teamRoles[0] != domain.TeamRoleMember {
			t.Fatalf("team roles = %v, want one member", teamRoles)
		}
	})
}

func TestRequesterInviteKeepsExistingIdentities(t *testing.T) {
	t.Run("an existing person keeps its kind and memberships", func(t *testing.T) {
		d := newRequesterDB(t)
		repo := d.repo()
		beforeOrg, beforeTeam := d.memberships(d.lead)
		// A requester invitation for an email that already has an account is
		// refused by the service, so write the row directly: the acceptance
		// path must still not convert the person.
		teamID := d.team
		if _, err := repo.CreateInvite(d.ctx, domain.Invite{
			Email: "lead@requester.test", OrgID: d.org, OrgRole: domain.OrgRoleMember,
			TeamID: &teamID, TeamRole: domain.TeamRoleMember, Kind: domain.InviteKindRequester,
		}); err != nil {
			t.Fatal(err)
		}
		// Their next sign-in runs the same provisioning path.
		u, _ := d.signIn("rq-lead", "lead@requester.test", "Lead")
		if u.Kind != domain.UserKindHuman {
			t.Fatalf("an existing person became %q", u.Kind)
		}
		afterOrg, afterTeam := d.memberships(d.lead)
		if strings.Join(afterOrg, ",") != strings.Join(beforeOrg, ",") || strings.Join(afterTeam, ",") != strings.Join(beforeTeam, ",") {
			t.Fatalf("memberships changed: org %v -> %v, team %v -> %v", beforeOrg, afterOrg, beforeTeam, afterTeam)
		}
	})

	t.Run("an agent account is never converted", func(t *testing.T) {
		d := newRequesterDB(t)
		repo := d.repo()
		agentID := d.insert(`INSERT INTO users (subject, email, kind) VALUES ('rq-agent', 'bot@agents.amp', 'agent') RETURNING id`)
		teamID := d.team
		// The service refuses this too; write it directly to prove the
		// provisioning INSERT cannot touch an existing row's kind.
		if _, err := repo.CreateInvite(d.ctx, domain.Invite{
			Email: "bot@agents.amp", OrgID: d.org, OrgRole: domain.OrgRoleMember,
			TeamID: &teamID, TeamRole: domain.TeamRoleMember, Kind: domain.InviteKindRequester,
		}); err != nil {
			t.Fatal(err)
		}
		u, _ := d.signIn("rq-agent", "bot@agents.amp", "bot")
		if u.Kind != domain.UserKindAgent {
			t.Fatalf("an agent account became %q", u.Kind)
		}
		if got := d.kindOf(agentID); got != domain.UserKindAgent {
			t.Fatalf("stored agent kind = %q", got)
		}
		orgRoles, teamRoles := d.memberships(agentID)
		if len(orgRoles) != 0 || len(teamRoles) != 0 {
			t.Fatalf("a requester invite granted an agent membership: org %v, team %v", orgRoles, teamRoles)
		}
	})

	t.Run("the service refuses a requester invite for an existing account", func(t *testing.T) {
		d := newRequesterDB(t)
		svc := d.svc()
		teamID := d.team
		_, err := svc.Invite(d.ctx, d.principal(d.lead), domain.Invite{
			Email: "lead@requester.test", OrgID: d.org, TeamID: &teamID, Kind: domain.InviteKindRequester,
		})
		if err == nil {
			t.Fatal("a requester invite was issued for an email that already has an account")
		}
		if !strings.Contains(err.Error(), "already has an AMP account") {
			t.Fatalf("refusal does not say why: %v", err)
		}
		if n := d.pendingInvites("lead@requester.test"); n != 0 {
			t.Fatalf("refused invitation still wrote %d pending row(s)", n)
		}
	})

	t.Run("a customer can't be given team membership by a membership invite", func(t *testing.T) {
		d := newRequesterDB(t)
		repo := d.repo()
		customer := d.insert(`INSERT INTO users (subject, email, kind) VALUES ('rq-cust', 'cust@requester.test', 'customer') RETURNING id`)
		teamID := d.team
		// A membership invite that names the team would otherwise add a
		// team_members row — and team membership is how a person reaches a
		// project. The acceptance path refuses it and leaves it pending.
		inv, err := repo.CreateInvite(d.ctx, domain.Invite{
			Email: "cust@requester.test", OrgID: d.org, OrgRole: domain.OrgRoleMember,
			TeamID: &teamID, TeamRole: domain.TeamRoleMember,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.AcceptInvitesList(d.ctx, customer, "cust@requester.test"); err == nil {
			t.Fatal("a customer accepted team membership through an invite")
		}
		orgRoles, teamRoles := d.memberships(customer)
		if len(teamRoles) != 0 || len(orgRoles) != 0 {
			t.Fatalf("refused invite still granted org %v, team %v", orgRoles, teamRoles)
		}
		var accepted *time.Time
		if err := d.db.QueryRow(d.ctx, `SELECT accepted_at FROM invites WHERE id = $1`, inv.ID).Scan(&accepted); err != nil {
			t.Fatal(err)
		}
		if accepted != nil {
			t.Fatal("the refused invite was consumed; it must stay pending")
		}
	})
	t.Run("a customer whose Dex subject is recreated keeps its customer identity", func(t *testing.T) {
		d := newRequesterDB(t)
		// A customer already signed in once.
		first, _ := d.signIn("sub-cust-orig", "cust-signin@requester.test", "Requester")
		if first.Kind != domain.UserKindHuman {
			t.Fatalf("a first sign-in with no requester invite is not a person: %q", first.Kind)
		}
		d.exec(`UPDATE users SET kind = 'customer' WHERE id = $1`, first.ID)
		// Their Dex identity is recreated with a new subject but the same
		// email: the fallback path takes the row over instead of failing on
		// the email unique key, and it must not change the kind.
		again, _ := d.signIn("sub-cust-new", "cust-signin@requester.test", "Requester")
		if again.ID != first.ID {
			t.Fatalf("a recreated subject created a second row: %d vs %d", again.ID, first.ID)
		}
		if again.Kind != domain.UserKindCustomer {
			t.Fatalf("a customer's kind became %q after a subject change", again.Kind)
		}
		// The agent rule still holds: a sign-in never takes over an agent row.
		agent := d.insert(`INSERT INTO users (subject, email, kind) VALUES ('cust-bot', 'recreated-bot@agents.amp', 'agent') RETURNING id`)
		if _, _, err := d.repo().ProvisionUserFromClaims(d.ctx, "cust-bot-new", "recreated-bot@agents.amp", "bot", nil); err == nil {
			t.Fatal("a sign-in took over an agent account")
		}
		if got := d.kindOf(agent); got != domain.UserKindAgent {
			t.Fatalf("agent kind = %q", got)
		}
	})
}

// ---- Replay, revoke and foreign invitations ----

func TestRequesterInviteReplayAndRevoke(t *testing.T) {
	t.Run("replay replaces the pending intent instead of adding a row", func(t *testing.T) {
		d := newRequesterDB(t)
		repo := d.repo()
		teamID := d.team
		first, err := repo.CreateInvite(d.ctx, domain.Invite{
			Email: "replay@new.test", OrgID: d.org, OrgRole: domain.OrgRoleMember,
			TeamID: &teamID, TeamRole: domain.TeamRoleMember, Kind: domain.InviteKindRequester,
		})
		if err != nil {
			t.Fatal(err)
		}
		again, err := repo.CreateInvite(d.ctx, domain.Invite{
			Email: "REPLAY@new.test", OrgID: d.org, OrgRole: domain.OrgRoleMember,
			TeamID: &teamID, TeamRole: domain.TeamRoleMember, Kind: domain.InviteKindRequester,
		})
		if err != nil {
			t.Fatal(err)
		}
		if again.ID != first.ID {
			t.Fatalf("replay created a second invite %d (first %d)", again.ID, first.ID)
		}
		if n := d.pendingInvites("replay@new.test"); n != 1 {
			t.Fatalf("%d pending rows after a replay, want 1", n)
		}
		// Last invitation wins: re-inviting as a member makes it a membership
		// invite, and the first sign-in provisions a person, not a customer.
		if _, err := repo.CreateInvite(d.ctx, domain.Invite{
			Email: "replay@new.test", OrgID: d.org, OrgRole: domain.OrgRoleMember,
			TeamID: &teamID, TeamRole: domain.TeamRoleMember, Kind: domain.InviteKindMember,
		}); err != nil {
			t.Fatal(err)
		}
		u, _ := d.signIn("sub-replay", "replay@new.test", "Replay")
		if u.Kind != domain.UserKindHuman {
			t.Fatalf("kind = %q after the invitation was replaced by a membership invite", u.Kind)
		}
	})

	t.Run("a revoked requester invite can never be accepted", func(t *testing.T) {
		d := newRequesterDB(t)
		svc := d.svc()
		teamID := d.team
		res, err := svc.Invite(d.ctx, d.principal(d.lead), domain.Invite{
			Email: "revoked@new.test", OrgID: d.org, TeamID: &teamID, Kind: domain.InviteKindRequester,
		})
		if err != nil || !res.Invited {
			t.Fatalf("issue: %+v %v", res, err)
		}
		// The org admin revokes it.
		if err := svc.RevokeInvite(d.ctx, d.principal(d.admin), d.org, res.Invite.ID); err != nil {
			t.Fatal(err)
		}
		if n := d.pendingInvites("revoked@new.test"); n != 0 {
			t.Fatalf("%d pending rows after revoke", n)
		}
		u, pv := d.signIn("sub-revoked", "revoked@new.test", "Revoked")
		if u.Kind != domain.UserKindHuman {
			t.Fatalf("a revoked requester invite still provisioned %q", u.Kind)
		}
		if len(pv.Invites) != 0 {
			t.Fatalf("a revoked invite was accepted: %+v", pv.Invites)
		}
	})

	t.Run("a stale membership invite for the same email never becomes a customer's team membership", func(t *testing.T) {
		d := newRequesterDB(t)
		repo := d.repo()
		otherTeam := d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Elsewhere') RETURNING id`, d.org)
		// The org's admin invited this email into another team as a member;
		// the team lead also issued a requester invite. The requester intent
		// decides the kind, and the membership invite must still not grant a
		// customer team membership (which reaches projects). Like #3766's
		// owner-invite rule, it is refused loudly and left pending, so the
		// admin can revoke it — never silently granted.
		if _, err := repo.CreateInvite(d.ctx, domain.Invite{
			Email: "mixed@new.test", OrgID: d.org, OrgRole: domain.OrgRoleMember,
			TeamID: &otherTeam, TeamRole: domain.TeamRoleMember,
		}); err != nil {
			t.Fatal(err)
		}
		teamID := d.team
		if _, err := repo.CreateInvite(d.ctx, domain.Invite{
			Email: "mixed@new.test", OrgID: d.org, OrgRole: domain.OrgRoleMember,
			TeamID: &teamID, TeamRole: domain.TeamRoleMember, Kind: domain.InviteKindRequester,
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := repo.ProvisionUserFromClaims(d.ctx, "sub-mixed", "mixed@new.test", "Mixed", nil); err == nil {
			t.Fatal("sign-in succeeded although a customer would have gained team membership")
		} else if !strings.Contains(err.Error(), "customer") {
			t.Fatalf("refusal does not name the customer rule: %v", err)
		}
		// The identity that was provisioned is a customer, and it holds no
		// membership: no org row, no team row, no project access.
		id := d.userIDByEmail("mixed@new.test")
		if got := d.kindOf(id); got != domain.UserKindCustomer {
			t.Fatalf("provisioned kind = %q, want customer", got)
		}
		orgRoles, teamRoles := d.memberships(id)
		if len(orgRoles) != 0 || len(teamRoles) != 0 {
			t.Fatalf("a customer gained org %v / team %v", orgRoles, teamRoles)
		}
		if n := d.projectAccess(id); n != 0 {
			t.Fatalf("a customer reaches %d projects, want 0", n)
		}
		// Both invitations are still pending, so an admin can revoke the
		// membership one and the requester can then sign in.
		if n := d.pendingInvites("mixed@new.test"); n != 2 {
			t.Fatalf("%d pending rows, want 2 (nothing silently consumed)", n)
		}
		// Revoking the membership invite lets the requester in.
		var membershipInvite int
		if err := d.db.QueryRow(d.ctx,
			`SELECT id FROM invites WHERE lower(email) = lower($1) AND accepted_at IS NULL AND kind = 'member'`,
			"mixed@new.test").Scan(&membershipInvite); err != nil {
			t.Fatal(err)
		}
		if err := repo.DeleteInvite(d.ctx, membershipInvite); err != nil {
			t.Fatal(err)
		}
		u, pv := d.signIn("sub-mixed", "mixed@new.test", "Mixed")
		if u.Kind != domain.UserKindCustomer || len(pv.Invites) != 1 || !pv.Invites[0].IsRequester() {
			t.Fatalf("after revoking the membership invite: kind %q, invites %+v", u.Kind, pv.Invites)
		}
		if n := d.projectAccess(u.ID); n != 0 {
			t.Fatalf("a customer reaches %d projects after accepting, want 0", n)
		}
	})

}

func TestRequesterInviteAuthorization(t *testing.T) {
	t.Run("a plain org member can't issue a requester invite", func(t *testing.T) {
		d := newRequesterDB(t)
		svc := d.svc()
		teamID := d.team
		_, err := svc.Invite(d.ctx, d.principal(d.outsider), domain.Invite{
			Email: "nope@new.test", OrgID: d.org, TeamID: &teamID, Kind: domain.InviteKindRequester,
		})
		if err == nil {
			t.Fatal("a plain org member issued a requester invitation")
		}
		if n := d.pendingInvites("nope@new.test"); n != 0 {
			t.Fatalf("refused invitation wrote %d pending row(s)", n)
		}
	})

	t.Run("the org admin can issue one (RequireTeamManager)", func(t *testing.T) {
		d := newRequesterDB(t)
		svc := d.svc()
		teamID := d.team
		res, err := svc.Invite(d.ctx, d.principal(d.admin), domain.Invite{
			Email: "admin-invited@new.test", OrgID: d.org, TeamID: &teamID, Kind: domain.InviteKindRequester,
		})
		if err != nil || !res.Invited {
			t.Fatalf("org admin issuing a requester invite: %+v %v", res, err)
		}
		if got := d.pendingInvites("admin-invited@new.test"); got != 1 {
			t.Fatalf("%d pending rows, want 1", got)
		}
	})

	t.Run("a requester invite must name a team", func(t *testing.T) {
		d := newRequesterDB(t)
		svc := d.svc()
		_, err := svc.Invite(d.ctx, d.principal(d.admin), domain.Invite{
			Email: "noteam@new.test", OrgID: d.org, Kind: domain.InviteKindRequester,
		})
		if err == nil {
			t.Fatal("a requester invite without a team was accepted")
		}
		if !strings.Contains(err.Error(), "team") {
			t.Fatalf("refusal does not name the missing team: %v", err)
		}
	})

	t.Run("a requester invite can't smuggle an org or team role", func(t *testing.T) {
		d := newRequesterDB(t)
		svc := d.svc()
		teamID := d.team
		for _, inv := range []domain.Invite{
			{Email: "role1@new.test", OrgID: d.org, TeamID: &teamID, OrgRole: domain.OrgRoleAdmin, Kind: domain.InviteKindRequester},
			{Email: "role2@new.test", OrgID: d.org, TeamID: &teamID, TeamRole: domain.TeamRoleLead, Kind: domain.InviteKindRequester},
		} {
			if _, err := svc.Invite(d.ctx, d.principal(d.admin), inv); err == nil {
				t.Fatalf("a requester invite granted a role: %+v", inv)
			}
		}
	})

	t.Run("a requester invite is bound to its org and team", func(t *testing.T) {
		d := newRequesterDB(t)
		svc := d.svc()
		// A team belonging to another org.
		otherOrg := d.insert(`INSERT INTO orgs (name, slug) VALUES ('Other org', 'other-requester-org') RETURNING id`)
		otherTeam := d.insert(`INSERT INTO teams (org_id, name) VALUES ($1, 'Other inbox') RETURNING id`, otherOrg)
		// This org's admin can't bind the invitation to a team they don't manage.
		_, err := svc.Invite(d.ctx, d.principal(d.admin), domain.Invite{
			Email: "foreign@new.test", OrgID: d.org, TeamID: &otherTeam, Kind: domain.InviteKindRequester,
		})
		if err == nil {
			t.Fatal("a requester invite was bound to a team in another org")
		}
		if n := d.pendingInvites("foreign@new.test"); n != 0 {
			t.Fatalf("refused invitation wrote %d pending row(s)", n)
		}
		// And that org's admin can't issue into this org at all.
		if _, err := svc.Invite(d.ctx, d.principal(d.lead), domain.Invite{
			Email: "foreign2@new.test", OrgID: otherOrg, TeamID: &otherTeam, Kind: domain.InviteKindRequester,
		}); err == nil {
			t.Fatal("a principal issued a requester invite into an org they don't manage")
		}
	})

	t.Run("an unknown invite kind is refused", func(t *testing.T) {
		d := newRequesterDB(t)
		svc := d.svc()
		teamID := d.team
		_, err := svc.Invite(d.ctx, d.principal(d.admin), domain.Invite{
			Email: "kind@new.test", OrgID: d.org, TeamID: &teamID, Kind: "customer",
		})
		if err == nil {
			t.Fatal("an unknown invite kind was accepted")
		}
	})
}

// ---- The domain's normalization ----

func TestRequesterInviteKindDomain(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", domain.InviteKindMember, false},
		{"member", domain.InviteKindMember, false},
		{" Member ", domain.InviteKindMember, false},
		{"requester", domain.InviteKindRequester, false},
		{" Requester\t", domain.InviteKindRequester, false},
		{"customer", "", true},
		{"owner", "", true},
	}
	for _, c := range cases {
		got, err := domain.NormalizeInviteKind(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeInviteKind(%q) = %q, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeInviteKind(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeInviteKind(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if !(domain.Invite{Kind: domain.InviteKindRequester}).IsRequester() {
		t.Error("a requester invite does not report IsRequester")
	}
	if (domain.Invite{}).IsRequester() {
		t.Error("a zero invite reports IsRequester")
	}
}
