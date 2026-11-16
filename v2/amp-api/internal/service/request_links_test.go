package service

// Tests for request_links.go, the authorized attach path (AMP #3962, epic 202
// story 549; build slice E1). The repository's write path is already proven by
// request_links_test.go (#3875); these tests own what the SERVICE adds:
//
//  1. authorization of the caller. Attaching is triage — a team member or lead,
//     an org owner/admin or a global admin, resolved from the rows on every call
//     — and a requester, a customer with no standing, another team's member, an
//     agent and a submit-only token are each refused in the way their standing
//     deserves: a 404 that names nothing for anyone with no view, a 403 naming
//     the rule for the requester who may already see the request.
//  2. authorization of the TARGET, which is the ticket's "validate target actual
//     tenant/access, not client metadata" rule:
//     - a target in another org is answered exactly like a target that does not
//       exist (404, nothing written) and metadata claiming otherwise is ignored
//       (a request row patched to name the target's org, and a project patched to
//       claim the request's org, both change nothing);
//     - a target in this org whose PROJECT the caller cannot see is the same 404
//       — being in the org is not being allowed to see the work, and a link names
//       the id it points at;
//     - a target that no longer exists, and a project with no org at all, are the
//       same 404 — never an internal error.
//  3. one active route and honest history (D9). A first routes_to is the active
//     route; a second project without SupersedeRoute is a 409 naming the current
//     project and writes nothing; a supersede appends and keeps the old routing
//     as lineage; a replay writes nothing at all (no second row, no history row,
//     no audit row, no re-stamp of planned_at).
//  4. linking never approves, dispatches or schedules anything: after a
//     planned_as attach to a real ticket, that ticket's approved_at is still NULL
//     and Service.Dispatch still refuses it through the real plan-approval gate.
//
// Run against a disposable Postgres with every migration applied:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:PORT/amp_test?sslmode=disable \
//	go test ./internal/service -run 'AttachRequest|RequestLinkTarget|RequestAttach' -count=1 -race
//
// The fixture is request_links_test.go's lifecycleFixture (orgs, teams, users, a
// project hierarchy and requests, all with a run-unique suffix), reused rather
// than copied so the two request suites cannot drift apart.

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

// linkFixture is a lifecycleFixture plus what an attach needs beyond it: a
// SECOND project in the same org that the fixture's team is NOT on (so "in the
// org, not yours" is reachable), an artifact and a receipt in the visible
// project, and a team member who is on that second project but not on the inbox
// team.
type linkFixture struct {
	*lifecycleFixture

	hiddenProject int // in this org, no team of the caller's on it
	hiddenEpic    int
	hiddenStory   int
	hiddenTask    int

	noOrgProject int // a pre-004 project with no org: no tenant to verify
	noOrgTask    int

	artifact int
	receipt  int

	outsider int // on hiddenProject's owner team only: sees the work, not the inbox
}

func newLinkFixture(t *testing.T) *linkFixture {
	t.Helper()
	f := &linkFixture{lifecycleFixture: newLifecycleFixture(t)}
	q := func(sql string, args ...any) int {
		t.Helper()
		var id int
		if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	suffix := rand.IntN(1_000_000_000)

	// A second project in the fixture's own org, owned by a team the inbox team is
	// not on: same tenant, none of the inbox team's business. The outsider joins
	// that team, so they see the work and not the inbox.
	// Full suffixes, not truncations: projects.code is globally UNIQUE and this
	// fixture builds three projects, so a shortened code is a cross-run collision
	// waiting for a busy database (it happened once in 20 local runs).
	f.hiddenProject = q(`INSERT INTO projects (name, code, org_id) VALUES ('Hidden work', $1, $2) RETURNING id`,
		fmt.Sprintf("HID%d", suffix), f.org)
	f.outsider = q(`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
		fmt.Sprintf("link-outside-%d", suffix), fmt.Sprintf("link-outside-%d@link.test", suffix), domain.UserKindHuman)
	hiddenTeam := q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Hidden team') RETURNING id`, f.org)
	exec(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'owner')`, f.hiddenProject, hiddenTeam)
	exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, hiddenTeam, f.outsider)
	f.hiddenEpic = q(`INSERT INTO epics (project_id, name) VALUES ($1, 'Hidden epic') RETURNING id`, f.hiddenProject)
	f.hiddenStory = q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'Hidden story') RETURNING id`,
		f.hiddenProject, f.hiddenEpic)
	f.hiddenTask = q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'Hidden task') RETURNING id`,
		f.hiddenProject, f.hiddenEpic, f.hiddenStory)

	// A project that predates orgs (004): its org_id is NULL, so it has no tenant
	// to verify and a link to work inside it cannot be proved to be in this
	// request's tenant. The repository treats it as a missing target; this service
	// must too.
	f.noOrgProject = q(`INSERT INTO projects (name, code) VALUES ('No org work', $1) RETURNING id`,
		fmt.Sprintf("NOO%d", suffix))
	noOrgEpic := q(`INSERT INTO epics (project_id, name) VALUES ($1, 'No org epic') RETURNING id`, f.noOrgProject)
	noOrgStory := q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'No org story') RETURNING id`,
		f.noOrgProject, noOrgEpic)
	f.noOrgTask = q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'No org task') RETURNING id`,
		f.noOrgProject, noOrgEpic, noOrgStory)

	// An artifact and a receipt in the project the inbox team can see: the two
	// delivered_by target kinds that are not tickets.
	f.artifact = q(`INSERT INTO artifacts (project_id, title, kind) VALUES ($1, 'The mockup', 'html') RETURNING id`, f.project)
	f.receipt = q(`INSERT INTO task_receipts (task_id, project_id, summary, state) VALUES ($1, $2, 'done', 'final') RETURNING id`,
		f.task, f.project)
	return f
}

// attach is the service call under test, with the fixture's own shapes filled in.
func (f *linkFixture) attach(p authz.Principal, req *domain.TeamRequest, kind domain.RequestLinkKind,
	target domain.RequestTargetKind, targetID int) (*AttachRequestResult, error) {
	f.t.Helper()
	return f.svc.AttachRequest(f.ctx, p, AttachRequestInput{
		RequestID:  req.ID,
		TeamID:     req.TeamID,
		LinkKind:   kind,
		TargetKind: target,
		TargetID:   targetID,
	})
}

// linkRows reads the request's stored relations straight from the table, so a
// refusal's "wrote nothing" is checked against the row and not against a return
// value.
func (f *linkFixture) linkRows(requestID int) []domain.RequestLink {
	f.t.Helper()
	rows, err := f.db.Query(f.ctx, `SELECT id, request_id, link_kind, target_kind, target_id, created_by, created_at
		FROM request_links WHERE request_id = $1 ORDER BY id`, requestID)
	if err != nil {
		f.t.Fatalf("links of request %d: %v", requestID, err)
	}
	defer rows.Close()
	out := []domain.RequestLink{}
	for rows.Next() {
		var l domain.RequestLink
		if err := rows.Scan(&l.ID, &l.RequestID, &l.LinkKind, &l.TargetKind, &l.TargetID, &l.CreatedBy, &l.CreatedAt); err != nil {
			f.t.Fatalf("scan link of request %d: %v", requestID, err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("links of request %d: %v", requestID, err)
	}
	return out
}

// linkAudits is every `request.link` audit row targeting requestID.
func (f *linkFixture) linkAudits(requestID int) []domain.AuditEntry {
	f.t.Helper()
	entries, err := f.svc.Repo.ListAudit(f.ctx, domain.AuditFilter{Action: "request.link", TargetID: idStr(requestID)})
	if err != nil {
		f.t.Fatalf("list request.link audit for %d: %v", requestID, err)
	}
	return entries
}

// plannedAt reads the request's planned_at, the first-write-wins stamp a first
// planned_as link sets.
func (f *linkFixture) plannedAt(requestID int) *string {
	f.t.Helper()
	var at *string
	if err := f.db.QueryRow(f.ctx, `SELECT planned_at::text FROM team_requests WHERE id = $1`, requestID).Scan(&at); err != nil {
		f.t.Fatalf("read planned_at of %d: %v", requestID, err)
	}
	return at
}

// assertNothingLinked proves a refusal wrote nothing: no link row, no history row,
// no audit row and no planned_at stamp — the four things an attach can write. It
// takes what the request had before the refused act, because a request may
// legitimately have acquired links earlier in the same test.
func (f *linkFixture) assertNothingLinked(req *domain.TeamRequest, before linkSnapshot) {
	f.t.Helper()
	if rows := len(f.linkRows(req.ID)); rows != before.links {
		f.t.Errorf("a refused attach left %d link rows, want %d", rows, before.links)
	}
	if got := f.events(req.ID); got != before.events {
		f.t.Errorf("a refused attach left %d history rows, want %d", got, before.events)
	}
	if rows := len(f.linkAudits(req.ID)); rows != before.audits {
		f.t.Errorf("a refused attach left %d request.link audit rows, want %d", rows, before.audits)
	}
	if at := f.plannedAt(req.ID); (at == nil) != (before.plannedAt == nil) ||
		(at != nil && *at != *before.plannedAt) {
		f.t.Errorf("a refused attach moved planned_at to %v, want %v", at, before.plannedAt)
	}
}

// linkSnapshot is the four things an attach can write, read together so "a
// refusal wrote nothing" is one comparison.
type linkSnapshot struct {
	links     int
	events    int
	audits    int
	plannedAt *string
}

func (f *linkFixture) snapshot(reqID int) linkSnapshot {
	f.t.Helper()
	return linkSnapshot{
		links:     len(f.linkRows(reqID)),
		events:    f.events(reqID),
		audits:    len(f.linkAudits(reqID)),
		plannedAt: f.plannedAt(reqID),
	}
}

// ---- 1. Who may attach ----

// TestAttachRequestAuthorizesTheCaller is the caller half of criterion 1: the team
// triages, and everybody else is refused in the way their standing deserves.
func TestAttachRequestAuthorizesTheCaller(t *testing.T) {
	f := newLinkFixture(t)
	requester := f.customer
	req := f.newRequest("attachwho0", domain.RequestStatusTriaging, &requester)

	// Who may attach: a team member, a team lead and an org owner. Each is checked
	// through the same capability resolution the lifecycle uses, so this is the
	// contract's triage row and not a second rule.
	for name, who := range map[string]int{
		"a team member": f.member,
		"a team lead":   f.lead,
		"an org owner":  f.admin,
	} {
		t.Run(name, func(t *testing.T) {
			// A different target per caller so each one has something to write.
			res, err := f.attach(f.principal(who), req, domain.RequestLinkPlannedAs, domain.RequestTargetTask, f.task)
			if err != nil {
				t.Fatalf("%s could not attach: %v", name, err)
			}
			if res.Changed {
				// The first caller wrote it; later callers are replays of the same
				// relation, which the next test owns. Either answer is correct here.
				return
			}
			if !res.Changed && res.Link == nil {
				t.Fatalf("%s: replay answered with no link", name)
			}
		})
	}
	if n := len(f.linkRows(req.ID)); n != 1 {
		t.Fatalf("link rows = %d, want 1: the three callers share one relation", n)
	}

	// The requester may see their own request and may not decide what serves it:
	// a 403 that names the rule, and nothing written.
	req2 := f.newRequest("attachwho1", domain.RequestStatusTriaging, &requester)
	before2 := f.snapshot(req2.ID)
	got, err := f.attach(f.principal(requester), req2, domain.RequestLinkPlannedAs, domain.RequestTargetTask, f.task)
	if err == nil {
		t.Fatalf("the requester attached work to their own request: %+v", got)
	}
	if authz.StatusOf(err, 500) != 403 {
		t.Errorf("requester refusal = %d, want 403 (%v)", authz.StatusOf(err, 500), err)
	}
	if !strings.Contains(err.Error(), "requester") {
		t.Errorf("requester refusal %q does not say what the act belongs to", err)
	}
	f.assertNothingLinked(req2, before2)

	// Everyone with no standing on the request at all gets the ONE not-found, with
	// the same bytes for an id that exists and one that does not: a customer who is
	// not the requester, a member of a peer team, a member of a team in another org,
	// an agent account, a submit-only token (which is refused by its scope instead,
	// naming the limit), an unknown request id, and — because existence must not
	// leak — the same bytes for all of the not-found ones.
	outsider := f.newCustomer("attach-outsider")
	unknown := 987654321
	var notFoundMessages []string
	for _, tc := range []struct {
		name string
		p    authz.Principal
		id   int
		team int
	}{
		{"a customer with no standing", f.principal(outsider), req2.ID, f.team},
		{"a member of a sibling team", f.principal(f.otherTeamMember()), req2.ID, f.team},
		{"a member of a team in another org", f.principal(f.stranger), req2.ID, f.team},
		{"an agent account with no team role", f.agentPrincipal(), req2.ID, f.team},
		{"an unknown request id", f.principal(outsider), unknown, f.team},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.snapshot(req2.ID)
			got, err := f.svc.AttachRequest(f.ctx, tc.p, AttachRequestInput{
				RequestID: tc.id, TeamID: tc.team,
				LinkKind: domain.RequestLinkPlannedAs, TargetKind: domain.RequestTargetTask, TargetID: f.task,
			})
			if err == nil {
				t.Fatalf("%s attached work: %+v", tc.name, got)
			}
			if authz.CodeOf(err) != authz.CodeRequestNotFound || authz.StatusOf(err, 500) != 404 {
				t.Errorf("%s refusal = %d %q, want 404 %q (%v)",
					tc.name, authz.StatusOf(err, 500), authz.CodeOf(err), authz.CodeRequestNotFound, err)
			}
			if strings.Contains(err.Error(), fmt.Sprint(tc.id)) {
				t.Errorf("%s refusal names the request id: %q — a 404 must not leak existence", tc.name, err)
			}
			notFoundMessages = append(notFoundMessages, err.Error())
			f.assertNothingLinked(req2, before)
		})
	}
	for _, m := range notFoundMessages[1:] {
		if m != notFoundMessages[0] {
			t.Errorf("refusals differ (%q vs %q): a 404 must not depend on who asked or which id", m, notFoundMessages[0])
		}
	}

	// A submit-only token is refused by its scope, and the refusal names the
	// scope: its answer is about the credential, not about an id.
	scoped := authz.Principal{
		UserID: f.member, Email: f.emailOf(f.member), Channel: auth.ChannelToken, ViaToken: true,
		AuthMethod: auth.AuthAPIToken, Scope: domain.TokenScopeSubmit, ScopeTeamID: f.team,
	}
	if _, err := f.attach(scoped, req2, domain.RequestLinkPlannedAs, domain.RequestTargetTask, f.task); err == nil {
		t.Fatal("a submit-only token attached work")
	} else if authz.CodeOf(err) != authz.CodeTokenScopeDenied {
		t.Errorf("submit-only token refusal code = %q, want %q (%v)", authz.CodeOf(err), authz.CodeTokenScopeDenied, err)
	}
	f.assertNothingLinked(req2, before2)

	// A stale ref of an entirely different shape — a request id that never existed
	// — answers the same code, and a request in another team is not reachable
	// through this team's id at all.
	foreign := f.newRequestIn(f.otherTeam, "attachwho2", domain.RequestStatusTriaging, &requester)
	foreignBefore := f.snapshot(foreign.ID)
	if _, err := f.svc.AttachRequest(f.ctx, f.principal(f.member), AttachRequestInput{
		RequestID: foreign.ID, TeamID: f.team,
		LinkKind: domain.RequestLinkPlannedAs, TargetKind: domain.RequestTargetTask, TargetID: f.task,
	}); err == nil || authz.CodeOf(err) != authz.CodeRequestNotFound {
		t.Fatalf("another team's request through this team = %v, want request.not_found", err)
	}
	f.assertNothingLinked(foreign, foreignBefore)
}

// emailOf reads one user's email, for a principal the test builds by hand.
func (f *linkFixture) emailOf(userID int) string {
	f.t.Helper()
	var email string
	if err := f.db.QueryRow(f.ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		f.t.Fatalf("email of user %d: %v", userID, err)
	}
	return email
}

// otherTeamMember inserts a person who is only on the fixture's sibling team (the
// other inbox in the same org) and returns their id.
func (f *linkFixture) otherTeamMember() int {
	f.t.Helper()
	suffix := rand.IntN(1_000_000_000)
	var id int
	if err := f.db.QueryRow(f.ctx, `INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
		fmt.Sprintf("link-peer-%d", suffix), fmt.Sprintf("link-peer-%d@link.test", suffix), domain.UserKindHuman).Scan(&id); err != nil {
		f.t.Fatalf("insert peer member: %v", err)
	}
	if _, err := f.db.Exec(f.ctx, `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`,
		f.otherTeam, id); err != nil {
		f.t.Fatalf("join peer team: %v", err)
	}
	return id
}

// ---- 2. The target's real tenant and access ----

// TestAttachRequestValidatesTheTargetNotTheMetadata is the target half of
// criterion 1, and the ticket's own words: "validate target actual tenant/access,
// not client metadata".
func TestAttachRequestValidatesTheTargetNotTheMetadata(t *testing.T) {
	f := newLinkFixture(t)
	requester := f.customer
	req := f.newRequest("attachten0", domain.RequestStatusTriaging, &requester)

	// Metadata that claims otherwise, on both ends. A JSONB key is inherited and
	// patchable, so neither of these may change any decision below: the target's
	// table and the project's access rows are the only sources.
	if _, err := f.db.Exec(f.ctx, `UPDATE team_requests SET metadata = $2 WHERE id = $1`, req.ID,
		fmt.Sprintf(`{"org_id": %d, "target_org_id": %d, "project_id": %d}`, f.otherOrg, f.otherOrg, f.hiddenProject)); err != nil {
		t.Fatalf("patch request metadata: %v", err)
	}
	if _, err := f.db.Exec(f.ctx, `UPDATE projects SET metadata = $2 WHERE id = $1`, f.hiddenProject,
		fmt.Sprintf(`{"org_id": %d, "request_team_id": %d}`, f.org, f.team)); err != nil {
		t.Fatalf("patch project metadata: %v", err)
	}

	member := f.principal(f.member)
	cases := []struct {
		name       string
		kind       domain.RequestLinkKind
		target     domain.RequestTargetKind
		targetID   int
		wantStatus int // 0: allowed
	}{
		{"a ticket in this org on a project the caller is on", domain.RequestLinkPlannedAs, domain.RequestTargetTask, f.task, 0},
		{"an epic in this org on a project the caller is on", domain.RequestLinkPlannedAs, domain.RequestTargetEpic, f.epic, 0},
		{"a story in this org on a project the caller is on", domain.RequestLinkPlannedAs, domain.RequestTargetStory, f.story, 0},
		{"an artifact in this org on a project the caller is on", domain.RequestLinkDeliveredBy, domain.RequestTargetArtifact, f.artifact, 0},
		{"a receipt in this org on a project the caller is on", domain.RequestLinkDeliveredBy, domain.RequestTargetReceipt, f.receipt, 0},
		{"the project itself", domain.RequestLinkRoutesTo, domain.RequestTargetProject, f.project, 0},

		{"a ticket in ANOTHER org", domain.RequestLinkPlannedAs, domain.RequestTargetTask, f.otherTask, 404},
		{"a ticket on a project in this org the caller cannot see", domain.RequestLinkPlannedAs, domain.RequestTargetTask, f.hiddenTask, 404},
		{"an epic on a project the caller cannot see", domain.RequestLinkPlannedAs, domain.RequestTargetEpic, f.hiddenEpic, 404},
		{"a story on a project the caller cannot see", domain.RequestLinkPlannedAs, domain.RequestTargetStory, f.hiddenStory, 404},
		{"a project in this org the caller cannot see", domain.RequestLinkRoutesTo, domain.RequestTargetProject, f.hiddenProject, 404},
		{"a ticket on a project with no org: no tenant to verify", domain.RequestLinkPlannedAs, domain.RequestTargetTask, f.noOrgTask, 404},
		{"the project with no org itself", domain.RequestLinkRoutesTo, domain.RequestTargetProject, f.noOrgProject, 404},
		{"an id that names nothing", domain.RequestLinkPlannedAs, domain.RequestTargetTask, 1 << 30, 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := f.snapshot(req.ID)
			res, err := f.attach(member, req, tc.kind, tc.target, tc.targetID)
			if tc.wantStatus == 0 {
				if err != nil {
					t.Fatalf("attach: %v", err)
				}
				if res.Target == nil || res.Target.ID != tc.targetID {
					t.Fatalf("result target = %+v, want id %d", res.Target, tc.targetID)
				}
				if res.Target.OrgID != f.org {
					t.Errorf("resolved target org = %d, want %d (from the target's own table)", res.Target.OrgID, f.org)
				}
				return
			}
			if err == nil {
				t.Fatalf("attach was accepted (%+v), want a %d", res, tc.wantStatus)
			}
			if got := authz.StatusOf(err, 500); got != tc.wantStatus {
				t.Errorf("status = %d, want %d (%v)", got, tc.wantStatus, err)
			}
			f.assertNothingLinked(req, before)
			if strings.Contains(err.Error(), fmt.Sprint(tc.targetID)) && tc.targetID == f.hiddenTask {
				// Naming the target the CALLER asked for is fine; naming its project
				// would tell them something they may not see.
				if strings.Contains(err.Error(), fmt.Sprint(f.hiddenProject)) {
					t.Errorf("the refusal names a project the caller cannot see: %v", err)
				}
			}
		})
	}
	if got := len(f.linkRows(req.ID)); got != 6 {
		t.Errorf("link rows after the successful cases = %d, want 6", got)
	}

	// And one more attach still writes exactly three things: a link row, its
	// history row and one audit row — the count the successful cases above set up.
	before := f.snapshot(req.ID)
	if _, err := f.attach(member, req, domain.RequestLinkPlannedAs, domain.RequestTargetTask, f.task2); err != nil {
		t.Fatalf("one more attach: %v", err)
	}
	if after := f.snapshot(req.ID); after.links != before.links+1 || after.events != before.events+1 || after.audits != before.audits+1 {
		t.Errorf("one attach wrote links %d→%d, events %d→%d, audits %d→%d; want one of each",
			before.links, after.links, before.events, after.events, before.audits, after.audits)
	}
}

// TestAttachRequestRefusesTargetsThatNoLongerExist pins the tombstone boundary:
// a target deleted after it was linked leaves the link (055's no-FK rule), but a
// NEW attach to a deleted target is a 404 rather than a row pointing at nothing.
func TestAttachRequestRefusesTargetsThatNoLongerExist(t *testing.T) {
	f := newLinkFixture(t)
	req := f.newRequest("attachtomb0", domain.RequestStatusTriaging, nil)
	member := f.principal(f.member)

	if _, err := f.attach(member, req, domain.RequestLinkDeliveredBy, domain.RequestTargetTask, f.task2); err != nil {
		t.Fatalf("attach before deletion: %v", err)
	}
	if _, err := f.db.Exec(f.ctx, `DELETE FROM tasks WHERE id = $1`, f.task2); err != nil {
		t.Fatalf("delete the linked ticket: %v", err)
	}
	// The link is a tombstone now: readable, and a replay of the same relation is
	// still a replay (the repository's rule, proven here through the service).
	if res, err := f.attach(member, req, domain.RequestLinkDeliveredBy, domain.RequestTargetTask, f.task2); err != nil {
		t.Fatalf("replay after deletion: %v", err)
	} else if res.Changed {
		t.Error("a replay after deletion reported changed=true")
	}
	// A DIFFERENT relation to the deleted ticket is refused: there is nothing there
	// to attach now.
	if _, err := f.attach(member, req, domain.RequestLinkPlannedAs, domain.RequestTargetTask, f.task2); err == nil {
		t.Fatal("a new link to a deleted target was accepted")
	} else if authz.StatusOf(err, 500) != 404 {
		t.Errorf("new link to a deleted target = %d, want 404 (%v)", authz.StatusOf(err, 500), err)
	}
}

// ---- 3. One active route, honest history, and a replay that writes nothing ----

// TestAttachRequestKeepsOneActiveRouteAndWritesAReplayNothing is criterion 2.
func TestAttachRequestKeepsOneActiveRouteAndWritesAReplayNothing(t *testing.T) {
	f := newLinkFixture(t)
	req := f.newRequest("attachrout0", domain.RequestStatusTriaging, nil)
	member := f.principal(f.member)
	second := f.newLinkProject("route second")

	res, err := f.attach(member, req, domain.RequestLinkRoutesTo, domain.RequestTargetProject, f.project)
	if err != nil {
		t.Fatalf("first route: %v", err)
	}
	if !res.Changed {
		t.Fatal("the first route reported changed=false")
	}
	active, err := f.svc.Repo.ActiveRequestRoute(f.ctx, req.ID, f.org, f.team)
	if err != nil || active == nil || active.TargetID != f.project {
		t.Fatalf("active route = %+v err=%v, want project %d", active, err, f.project)
	}
	rows, events, audits := len(f.linkRows(req.ID)), f.events(req.ID), len(f.linkAudits(req.ID))
	planned := f.plannedAt(req.ID)

	// A second project without superseding is D9's refusal, names the current
	// project, and writes nothing at all.
	got, err := f.attach(member, req, domain.RequestLinkRoutesTo, domain.RequestTargetProject, second)
	if err == nil {
		t.Fatalf("a second route was accepted: %+v", got)
	}
	if authz.CodeOf(err) != authz.CodeRequestConflict {
		t.Errorf("re-route refusal code = %q, want %q (%v)", authz.CodeOf(err), authz.CodeRequestConflict, err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(f.project)) {
		t.Errorf("re-route refusal %q does not name the current project", err)
	}
	if len(f.linkRows(req.ID)) != rows || f.events(req.ID) != events || len(f.linkAudits(req.ID)) != audits {
		t.Error("a refused re-route wrote something")
	}

	// The replay of the active route writes nothing either — not one row, not one
	// history row, not one audit row, and no second planned_at.
	replay, err := f.attach(member, req, domain.RequestLinkRoutesTo, domain.RequestTargetProject, f.project)
	if err != nil {
		t.Fatalf("replayed route: %v", err)
	}
	if replay.Changed {
		t.Error("a replayed route reported changed=true")
	}
	if replay.Link == nil || replay.Link.ID != res.Link.ID {
		t.Errorf("a replay answered with link %+v, want the original %d", replay.Link, res.Link.ID)
	}
	if len(f.linkRows(req.ID)) != rows || f.events(req.ID) != events || len(f.linkAudits(req.ID)) != audits {
		t.Error("a replayed attach wrote something: one relation is one row, one event and one audit")
	}

	// A deliberate supersede appends: the new project becomes the active answer and
	// the old routing stays readable as lineage.
	sup := AttachRequestInput{
		RequestID: req.ID, TeamID: req.TeamID, LinkKind: domain.RequestLinkRoutesTo,
		TargetKind: domain.RequestTargetProject, TargetID: second, SupersedeRoute: true,
	}
	if res, err := f.svc.AttachRequest(f.ctx, member, sup); err != nil {
		t.Fatalf("superseding route: %v", err)
	} else if !res.Changed {
		t.Error("a superseding route reported changed=false")
	}
	active, err = f.svc.Repo.ActiveRequestRoute(f.ctx, req.ID, f.org, f.team)
	if err != nil || active == nil || active.TargetID != second {
		t.Fatalf("active route after supersede = %+v err=%v, want project %d", active, err, second)
	}
	all, err := f.svc.Repo.RequestLinks(f.ctx, req.ID, f.org, f.team, domain.RequestLinkRoutesTo)
	if err != nil {
		t.Fatalf("routing history: %v", err)
	}
	if len(all) != 2 || all[0].TargetID != f.project || all[1].TargetID != second {
		t.Fatalf("routing history = %+v, want project %d then %d in attach order", all, f.project, second)
	}
	if at := f.plannedAt(req.ID); (at == nil) != (planned == nil) || (at != nil && *at != *planned) {
		t.Errorf("a routes_to attach moved planned_at to %v, want %v: only a first planned_as stamps it", at, planned)
	}
}

// newLinkProject inserts one more project in the fixture's org, on the inbox
// team, so a re-route has a second legal destination.
func (f *linkFixture) newLinkProject(label string) int {
	f.t.Helper()
	suffix := rand.IntN(1_000_000_000)
	var id int
	if err := f.db.QueryRow(f.ctx,
		`INSERT INTO projects (name, code, org_id) VALUES ($1, $2, $3) RETURNING id`,
		label, fmt.Sprintf("LK%d", suffix), f.org).Scan(&id); err != nil {
		f.t.Fatalf("insert project %q: %v", label, err)
	}
	if _, err := f.db.Exec(f.ctx, `INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'member')`,
		id, f.team); err != nil {
		f.t.Fatalf("link project %d to the inbox team: %v", id, err)
	}
	return id
}

// ---- 4. Linking an existing plan item never approves or starts it ----

// TestAttachRequestNeverApprovesOrDispatches is criterion 3. It links a request to
// a real ticket in a project that requires per-ticket approval, and then proves
// through the real dispatch gate — not by reading this file's intentions — that
// the ticket is still awaiting its person and still cannot be started.
func TestAttachRequestNeverApprovesOrDispatches(t *testing.T) {
	f := newLinkFixture(t)
	req := f.newRequest("attachgate0", domain.RequestStatusTriaging, nil)
	member := f.principal(f.member)

	// The project needs each ticket approved before an agent may start it, and the
	// ticket has never been approved.
	if _, err := f.db.Exec(f.ctx, `UPDATE projects SET require_plan_approval = TRUE WHERE id = $1`, f.project); err != nil {
		t.Fatalf("turn on plan approval: %v", err)
	}
	if _, err := f.db.Exec(f.ctx, `UPDATE tasks SET approved_at = NULL, approved_by = NULL, approved_for = NULL,
		planned_at = NOW() WHERE id = $1`, f.task); err != nil {
		t.Fatalf("clear the ticket's approval: %v", err)
	}

	before, err := f.svc.Repo.GetTask(f.ctx, f.task)
	if err != nil {
		t.Fatalf("load the ticket: %v", err)
	}
	if before.Approved() {
		t.Fatal("the fixture's ticket is already approved: the gate would prove nothing")
	}
	if before.DispatchedAt != nil || before.DispatchID != "" || before.StartedAt != nil {
		t.Fatalf("the fixture's ticket is already dispatched (%+v)", before)
	}

	// Attach it as planned work and as a deliverable: two links, no plan change.
	for _, kind := range []domain.RequestLinkKind{domain.RequestLinkPlannedAs, domain.RequestLinkDeliveredBy} {
		if _, err := f.attach(member, req, kind, domain.RequestTargetTask, f.task); err != nil {
			t.Fatalf("attach %s: %v", kind, err)
		}
	}

	after, err := f.svc.Repo.GetTask(f.ctx, f.task)
	if err != nil {
		t.Fatalf("reload the ticket: %v", err)
	}
	if after.Approved() || after.ApprovedAt != nil {
		t.Errorf("attaching APPROVED ticket %d: approved_at = %v, want NULL", f.task, after.ApprovedAt)
	}
	if !after.Planned().Equal(before.Planned()) {
		t.Errorf("attaching re-planned ticket %d: planned %s → %s", f.task, before.Planned(), after.Planned())
	}
	if after.DispatchedAt != nil || after.DispatchID != "" || after.StartedAt != nil || after.State != before.State {
		t.Errorf("attaching dispatched ticket %d: %+v, want it untouched", f.task, after)
	}

	// The real gate: Dispatched still refuses the ticket, and it says a person has
	// to approve it — so no link turned into an approval.
	if _, err := f.svc.Dispatch(f.ctx, f.principal(f.lead), f.task, "amp:amp-worker"); err == nil {
		t.Fatal("a linked but unapproved ticket was dispatched: linking approved the plan")
	} else {
		if authz.StatusOf(err, 500) != 403 {
			t.Errorf("dispatch refusal = %d, want 403 (%v)", authz.StatusOf(err, 500), err)
		}
		if !strings.Contains(err.Error(), "approved") {
			t.Errorf("dispatch refusal %q does not name the approval gate", err)
		}
	}
	// And it is still unapproved in the row, so "the gate refused" is not hiding a
	// write that happened anyway.
	final, err := f.svc.Repo.GetTask(f.ctx, f.task)
	if err != nil {
		t.Fatalf("reload the ticket after the dispatch attempt: %v", err)
	}
	if final.Approved() || final.DispatchID != "" || final.DispatchedAt != nil {
		t.Errorf("the refused dispatch wrote to ticket %d: %+v", f.task, final)
	}

	// The request did not move either: an attach is lineage, not a lifecycle act.
	stored := f.get(req.ID, f.team)
	if stored.State != domain.RequestStatusTriaging || stored.Rev != req.Rev {
		t.Errorf("attaching moved the request: %s@%d, want %s@%d", stored.State, stored.Rev, req.State, req.Rev)
	}
}

// ---- The relation matrix and the input guards ----

// TestAttachRequestRefusesRelationsItDoesNotOwn pins the pairs this path must not
// write, with the code that says why: a matrix the contract never allows, a
// duplicate fold (MarkDuplicate's), a relation between two requests (its own act,
// outside this slice) and a target id that names nothing.
func TestAttachRequestRefusesRelationsItDoesNotOwn(t *testing.T) {
	f := newLinkFixture(t)
	req := f.newRequest("attachmx00", domain.RequestStatusTriaging, nil)
	member := f.principal(f.member)
	before := f.snapshot(req.ID)

	cases := []struct {
		name       string
		kind       domain.RequestLinkKind
		target     domain.RequestTargetKind
		targetID   int
		wantCode   string
		wantInText string
	}{
		{"planned_as to an artifact: not a contract pair", domain.RequestLinkPlannedAs,
			domain.RequestTargetArtifact, f.artifact, authz.CodeRequestInvalid, "points at"},
		{"routes_to to a ticket: not a contract pair", domain.RequestLinkRoutesTo,
			domain.RequestTargetTask, f.task, authz.CodeRequestInvalid, "points at"},
		{"duplicate_of: the fold owns it", domain.RequestLinkDuplicateOf,
			domain.RequestTargetRequest, req.ID + 1, authz.CodeRequestInvalid, "duplicate"},
		{"relates_to: not a work attach", domain.RequestLinkRelatesTo,
			domain.RequestTargetRequest, req.ID + 1, authz.CodeRequestInvalid, "another request"},
		{"a target id that names nothing", domain.RequestLinkPlannedAs,
			domain.RequestTargetTask, 0, authz.CodeRequestInvalid, "target_id"},
		{"an invented link kind", domain.RequestLinkKind("scheduled_as"),
			domain.RequestTargetTask, f.task, authz.CodeRequestInvalid, "link kind"},
		{"an invented target kind", domain.RequestLinkPlannedAs,
			domain.RequestTargetKind("branch"), f.task, authz.CodeRequestInvalid, "points at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.attach(member, req, tc.kind, tc.target, tc.targetID)
			if err == nil {
				t.Fatalf("attach was accepted, want request.invalid")
			}
			if authz.CodeOf(err) != tc.wantCode {
				t.Errorf("code = %q, want %q (%v)", authz.CodeOf(err), tc.wantCode, err)
			}
			if !strings.Contains(err.Error(), tc.wantInText) {
				t.Errorf("refusal %q does not mention %q", err, tc.wantInText)
			}
		})
	}
	f.assertNothingLinked(req, before)
}

// TestAttachRequestValidatesItsScope pins the input guard that is a caller bug
// rather than a refusal: a missing request or team id cannot name a tenant, and
// the attach says so instead of resolving a bare id.
func TestAttachRequestValidatesItsScope(t *testing.T) {
	f := newLinkFixture(t)
	req := f.newRequest("attachsc00", domain.RequestStatusTriaging, nil)
	member := f.principal(f.member)

	for name, in := range map[string]AttachRequestInput{
		"no request": {TeamID: f.team, LinkKind: domain.RequestLinkPlannedAs, TargetKind: domain.RequestTargetTask, TargetID: f.task},
		"no team":    {RequestID: req.ID, LinkKind: domain.RequestLinkPlannedAs, TargetKind: domain.RequestTargetTask, TargetID: f.task},
	} {
		if _, err := f.svc.AttachRequest(f.ctx, member, in); err == nil {
			t.Fatalf("%s: attach was accepted", name)
		} else if authz.StatusOf(err, 500) != 404 && authz.StatusOf(err, 500) != 400 {
			t.Errorf("%s: status = %d, want 404 or 400 (%v)", name, authz.StatusOf(err, 500), err)
		}
	}
	// A team id that does not exist is the same not-found a request in it would
	// be: one 404, no existence to leak.
	if _, err := f.svc.AttachRequest(f.ctx, member, AttachRequestInput{
		RequestID: req.ID, TeamID: 1 << 30,
		LinkKind: domain.RequestLinkPlannedAs, TargetKind: domain.RequestTargetTask, TargetID: f.task,
	}); err == nil || authz.CodeOf(err) != authz.CodeRequestNotFound {
		t.Fatalf("an unknown team = %v, want request.not_found", err)
	}
}

// linkKindForTarget is the contract pair a target kind belongs to: a project is
// what a request is ROUTED to, an artifact or receipt is what DELIVERED it, and
// an epic, story or task is what it was PLANNED as (055's matrix).
func linkKindForTarget(kind domain.RequestTargetKind) domain.RequestLinkKind {
	switch kind {
	case domain.RequestTargetProject:
		return domain.RequestLinkRoutesTo
	case domain.RequestTargetArtifact, domain.RequestTargetReceipt:
		return domain.RequestLinkDeliveredBy
	default:
		return domain.RequestLinkPlannedAs
	}
}

// TestResolveRequestLinkTargetCoversEveryTargetKind is the cross-package half of
// the resolver: every target kind the domain allows has a statement, and each one
// answers the project the target really lives in and that project's org — read
// from the target's own table, which is what LinkRequest re-verifies.
func TestResolveRequestLinkTargetCoversEveryTargetKind(t *testing.T) {
	f := newLinkFixture(t)
	for _, kind := range domain.RequestTargetKinds {
		if kind == domain.RequestTargetRequest {
			// A request target resolves to no project on purpose (it is routed to
			// one rather than living in one); its org still comes from its row.
			other := f.newRequest("attachrsv0", domain.RequestStatusTriaging, nil)
			got, err := f.svc.Repo.ResolveRequestLinkTarget(f.ctx, kind, other.ID)
			if err != nil {
				t.Fatalf("resolve %s: %v", kind, err)
			}
			if got.ProjectID != 0 || got.OrgID != f.org {
				t.Errorf("resolve %s = project %d org %d, want 0/%d", kind, got.ProjectID, got.OrgID, f.org)
			}
			continue
		}
		var id, wantProject int
		switch kind {
		case domain.RequestTargetProject:
			id, wantProject = f.project, f.project
		case domain.RequestTargetEpic:
			id, wantProject = f.epic, f.project
		case domain.RequestTargetStory:
			id, wantProject = f.story, f.project
		case domain.RequestTargetTask:
			id, wantProject = f.task, f.project
		case domain.RequestTargetArtifact:
			id, wantProject = f.artifact, f.project
		case domain.RequestTargetReceipt:
			id, wantProject = f.receipt, f.project
		}
		got, err := f.svc.Repo.ResolveRequestLinkTarget(f.ctx, kind, id)
		if err != nil {
			t.Fatalf("resolve %s %d: %v", kind, id, err)
		}
		if got.ProjectID != wantProject || got.OrgID != f.org {
			t.Errorf("resolve %s %d = project %d org %d, want %d/%d",
				kind, id, got.ProjectID, got.OrgID, wantProject, f.org)
		}
		// And the value this resolver produces is the one the write path accepts:
		// a mutation here (a wrong join) would make LinkRequest answer
		// ErrRequestLinkTargetIdentity instead of storing the row.
		req := f.newRequest(fmt.Sprintf("rsv%.6s", kind), domain.RequestStatusTriaging, nil)
		link, changed, err := f.svc.Repo.LinkRequest(f.ctx, repository.RequestLinkWrite{
			RequestID: req.ID, OrgID: f.org, TeamID: req.TeamID,
			LinkKind:   linkKindForTarget(kind),
			TargetKind: kind, TargetID: id, TargetOrgID: got.OrgID, Actor: "test",
		})
		if err != nil || !changed {
			t.Fatalf("LinkRequest with the resolved org for %s: changed=%v err=%v", kind, changed, err)
		}
		if link.TargetKind != kind || link.TargetID != id {
			t.Errorf("stored link = %+v, want %s %d", link, kind, id)
		}
	}
	// An unknown kind and a non-positive id are refused rather than looked up.
	if _, err := f.svc.Repo.ResolveRequestLinkTarget(f.ctx, domain.RequestTargetKind("branch"), 1); err == nil {
		t.Error("an unknown target kind resolved")
	}
	if _, err := f.svc.Repo.ResolveRequestLinkTarget(f.ctx, domain.RequestTargetTask, 0); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("target id 0 = %v, want ErrNotFound", err)
	}
}
