package main

// The inbox planning and attachment tools through the real MCP server (epic
// 202, story 549, AMP #3972; approved contract artifact 36 v2 §1/§3/§5; build
// slice I3).
//
// The three tools are thin adapters over service methods that
// internal/service/request_links_test.go, request_handoff_test.go and
// request_uploads_test.go already prove as boundaries. So this file asks only
// what is about *the tools*:
//
//   - does the call arrive at the same service, and therefore produce the same
//     rows REST's adapter will (one relation plus its history and audit entry,
//     a batch of tickets with their dependencies and their untrusted quotes, a
//     private file with its sniffed type and its scan state)?
//   - is the retry contract real through this transport: does a replayed attach
//     write nothing at all, and does a handoff retried with its own key plan no
//     second ticket?
//   - does authorization hold through this transport: a stranger learns nothing
//     (one 404 for unknown, foreign and other-team), a target in another org is
//     not there, and a requester plans nothing?
//   - are the ticket's limits enforced before anything is stored: 25 MiB per
//     file, 5 files per request, and no location to fetch (a url argument is
//     refused, not ignored)?
//   - is the negative boundary real: no tool can accept an outcome for a
//     requester, approve a plan or dispatch a ticket, and after the whole surface
//     has been exercised every created ticket is still awaiting approval.
//
// Criterion 1 — idempotent authorized link/handoff works:
//   TestMCPRequestPlanning_AttachIsIdempotentAndAuthorized,
//   TestMCPRequestPlanning_HandoffIsRetrySafe
// Criterion 2 — file limits and no-fetch enforced:
//   TestMCPRequestPlanning_UploadIsBoundedAndFetchesNothing
// Criterion 3 — no tool accepts an outcome for the requester:
//   TestMCPRequestPlanning_NoAcceptanceAndNoAutoApproval
//
// Every call goes through the same path an agent's does: JSON-RPC → MCP server
// (its real middleware included) → tool handler → service → postgres. Identities
// are built in process from users rows, exactly as the auth middleware builds
// them; no live credential and no live stack is involved.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/mcp"
)

// planningToolNames is the surface this slice ships, spelled out here so the
// end-to-end assertions (listed, scope-refused, no acceptance among them) read
// against a fixed list rather than against whatever happens to be registered.
var planningSurfaceToolNames = []string{
	"amp_attach_request",
	"amp_handoff_request",
	"amp_upload_request_file",
}

// planningWorld is the triage fixture plus what planning needs: a project the
// inbox team delivers into (with an epic and a story), a second project in the
// same org the inbox team is NOT on, a project in another org entirely, and a
// request to plan.
type planningWorld struct {
	*requestWorkWorld

	project     int // routed to, owned by the inbox team
	epic        int
	story       int
	hiddenProj  int // same org, a team the inbox team is not on
	hiddenEpic  int
	hiddenStory int
	hiddenTask  int
	foreignProj int // another org
	foreignTask int
}

// projectCode builds a globally unique project code for a fixture project.
// projects.code is UNIQUE across the whole database (001), so a code shortened
// to a fixed width collides the second a suite runs twice against one database —
// the trap #3962's annotation records for the service fixtures, here at a prefix
// plus the whole random suffix.
func projectCode(prefix, suffix string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return -1
	}, suffix)
	if len(clean) > 6 {
		clean = clean[len(clean)-6:]
	}
	return prefix + clean
}

func newPlanningWorld(t *testing.T) *planningWorld {
	t.Helper()
	w := &planningWorld{requestWorkWorld: newRequestWorkWorld(t)}
	ctx := context.Background()
	repo := w.s.repo
	sfx := uniq("planning")
	q := func(sql string, args ...any) int {
		t.Helper()
		var id int
		if err := w.db(t).QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}

	// The project the request is routed to. Created through the repository with
	// this comment's own org and the inbox team as its owner team, so the team's
	// member reaches it through the team link and nobody else does.
	// Full random suffixes, never truncations: projects.code is globally UNIQUE.
	proj, err := repo.CreateProject(ctx, domain.CreateProjectRequest{
		Name: "Delivered work", Code: projectCode("PW", sfx), OrgID: &w.org, TeamID: &w.team,
	})
	if err != nil {
		t.Fatalf("create the routed project: %v", err)
	}
	w.project = proj.ID
	epic, err := repo.CreateEpic(ctx, domain.CreateEpicRequest{ProjectID: w.project, Name: "the-plan"})
	if err != nil {
		t.Fatalf("create the routed epic: %v", err)
	}
	w.epic = epic.ID
	story, err := repo.CreateStory(ctx, domain.CreateStoryRequest{ProjectID: w.project, EpicID: w.epic, Name: "the-story"})
	if err != nil {
		t.Fatalf("create the routed story: %v", err)
	}
	w.story = story.ID

	// A project in the same org that the inbox team is not on: same tenant, none
	// of the inbox's business. Attaching into it must be refused as a target that
	// is not there — being in the org is not enough (E1's tenant/access pair).
	w.hiddenProj = q(`INSERT INTO projects (name, code, org_id) VALUES ('Hidden work', $1, $2) RETURNING id`,
		strings.ToUpper("PH"+sfx), w.org)
	hiddenTeam := q(`INSERT INTO teams (org_id, name) VALUES ($1, 'Hidden team') RETURNING id`, w.org)
	// Its membership is somebody else entirely: the point of this project is that
	// the inbox's own member can NOT see it, so adding the member here would make
	// the "same org, not yours" case unreachable.
	outsider := q(`INSERT INTO users (subject, email, kind) VALUES ($1, $2, $3) RETURNING id`,
		sfx+"-outsider", sfx+"-outsider@planning.test", domain.UserKindHuman)
	q(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member') RETURNING user_id`, hiddenTeam, outsider)
	q(`INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'owner') RETURNING project_id`, w.hiddenProj, hiddenTeam)
	w.hiddenEpic = q(`INSERT INTO epics (project_id, name) VALUES ($1, 'hidden-epic') RETURNING id`, w.hiddenProj)
	w.hiddenStory = q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'hidden-story') RETURNING id`,
		w.hiddenProj, w.hiddenEpic)
	w.hiddenTask = q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'hidden-task') RETURNING id`,
		w.hiddenProj, w.hiddenEpic, w.hiddenStory)

	// And a project in another org entirely.
	w.foreignProj = q(`INSERT INTO projects (name, code, org_id) VALUES ('Foreign work', $1, $2) RETURNING id`,
		projectCode("PF", sfx), w.otherOrg)
	fEpic := q(`INSERT INTO epics (project_id, name) VALUES ($1, 'foreign-epic') RETURNING id`, w.foreignProj)
	fStory := q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'foreign-story') RETURNING id`, w.foreignProj, fEpic)
	w.foreignTask = q(`INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'foreign-task') RETURNING id`,
		w.foreignProj, fEpic, fStory)
	return w
}

// attach routes through the tool.
func (w *planningWorld) attach(t *testing.T, who *auth.Identity, args map[string]interface{}) mcpCallResult {
	t.Helper()
	return w.tool(t, who, "amp_attach_request", args)
}

// attachArgs is one well-formed attach of the fixture's request.
func (w *planningWorld) attachArgs(kind, targetKind string, targetID int) map[string]interface{} {
	return map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(w.refID),
		"link_kind": kind, "target_kind": targetKind, "target_id": float64(targetID),
	}
}

// linkRows reads the request's stored relations straight from the table, so a
// refused act's "wrote nothing" is checked against the row and not a return value.
func (w *planningWorld) linkRows(t *testing.T, requestID int) []domain.RequestLink {
	t.Helper()
	rows, err := w.db(t).Query(context.Background(),
		`SELECT id, request_id, link_kind, target_kind, target_id FROM request_links WHERE request_id = $1 ORDER BY id`, requestID)
	if err != nil {
		t.Fatalf("links of request %d: %v", requestID, err)
	}
	defer rows.Close()
	var out []domain.RequestLink
	for rows.Next() {
		var l domain.RequestLink
		if err := rows.Scan(&l.ID, &l.RequestID, &l.LinkKind, &l.TargetKind, &l.TargetID); err != nil {
			t.Fatalf("scan link: %v", err)
		}
		out = append(out, l)
	}
	return out
}

// countRows is a one-number read of the database, for the "nothing was written"
// assertions.
func (w *planningWorld) countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := w.db(t).QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// taskRow is the part of a created ticket these tests assert on.
type taskRow struct {
	ID             int
	Name           string
	Description    string
	State          string
	OwnerUserID    *int
	ApprovedAt     *string
	AssignedTo     *string
	StartAt        *string
	DependencyIDs  []int
	MetadataHandof string
}

func (w *planningWorld) task(t *testing.T, id int) taskRow {
	t.Helper()
	row := taskRow{ID: id}
	if err := w.db(t).QueryRow(context.Background(),
		`SELECT name, description, state, owner_user_id, approved_at::text, assigned_to, start_at::text,
		        COALESCE(metadata->>'request_handoff', '')
		   FROM tasks WHERE id = $1`, id).
		Scan(&row.Name, &row.Description, &row.State, &row.OwnerUserID, &row.ApprovedAt, &row.AssignedTo, &row.StartAt, &row.MetadataHandof); err != nil {
		t.Fatalf("read ticket %d: %v", id, err)
	}
	rows, err := w.db(t).Query(context.Background(), `SELECT depends_on FROM task_dependencies WHERE task_id = $1`, id)
	if err != nil {
		t.Fatalf("dependencies of %d: %v", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var dep int
		if err := rows.Scan(&dep); err != nil {
			t.Fatal(err)
		}
		row.DependencyIDs = append(row.DependencyIDs, dep)
	}
	return row
}

// handoffItems turns a tool result's items into (ref → task id).
func handoffItems(t *testing.T, res map[string]interface{}) map[string]int {
	t.Helper()
	raw, _ := res["items"].([]interface{})
	out := map[string]int{}
	for _, entry := range raw {
		m, ok := entry.(map[string]interface{})
		if !ok {
			t.Fatalf("item is not an object: %v", entry)
		}
		ref, _ := m["ref"].(string)
		id, _ := m["task_id"].(float64)
		out[ref] = int(id)
	}
	return out
}

// ---- criterion 1a: an authorized, idempotent attach ----

// TestMCPRequestPlanning_AttachIsIdempotentAndAuthorized walks the relation
// matrix through the tool: routing works and is authorized against both the
// target's tenant and the caller's access to it; a replay writes nothing at all;
// a second route is a named conflict that supersede_route resolves deliberately;
// and every refusal leaves the table exactly as it was.
func TestMCPRequestPlanning_AttachIsIdempotentAndAuthorized(t *testing.T) {
	w := newPlanningWorld(t)

	t.Run("routes the request to the project", func(t *testing.T) {
		got := w.attach(t, w.member, w.attachArgs("routes_to", "project", w.project))
		if got.errMessage != "" {
			t.Fatalf("route refused: %s %v", got.errMessage, got.errData)
		}
		if changed, _ := got.result["changed"].(bool); !changed {
			t.Fatalf("the first route reported changed=false: %v", got.result)
		}
		link, _ := got.result["link"].(map[string]interface{})
		if link == nil {
			t.Fatalf("no link in the answer: %v", got.result)
		}
		if link["link_kind"] != "routes_to" || link["target_kind"] != "project" || int(link["target_id"].(float64)) != w.project {
			t.Fatalf("stored link = %v", link)
		}
		// The answer names what the request now points at, read from the target's
		// own row.
		target, _ := got.result["target"].(map[string]interface{})
		if target == nil || int(target["project_id"].(float64)) != w.project {
			t.Fatalf("answer does not resolve the target: %v", got.result)
		}
	})

	t.Run("a replay writes nothing at all", func(t *testing.T) {
		links := len(w.linkRows(t, w.refID))
		events := w.countRows(t, `SELECT count(*) FROM request_events WHERE request_id = $1`, w.refID)
		audits := w.auditCount(t, "request.link", w.refID)

		got := w.attach(t, w.member, w.attachArgs("routes_to", "project", w.project))
		if got.errMessage != "" {
			t.Fatalf("replay refused: %s %v", got.errMessage, got.errData)
		}
		if changed, _ := got.result["changed"].(bool); changed {
			t.Fatalf("the replay reported changed=true: %v", got.result)
		}
		if n := len(w.linkRows(t, w.refID)); n != links {
			t.Fatalf("%d links after a replay, want %d", n, links)
		}
		if n := w.countRows(t, `SELECT count(*) FROM request_events WHERE request_id = $1`, w.refID); n != events {
			t.Fatalf("%d history rows after a replay, want %d", n, events)
		}
		if n := w.auditCount(t, "request.link", w.refID); n != audits {
			t.Fatalf("%d audit rows after a replay, want %d", n, audits)
		}
	})

	t.Run("a second project is a conflict that supersede_route resolves", func(t *testing.T) {
		second, err := w.s.repo.CreateProject(context.Background(), domain.CreateProjectRequest{
			Name: "Another delivered project", Code: projectCode("PS", uniq("second")), OrgID: &w.org, TeamID: &w.team,
		})
		if err != nil {
			t.Fatalf("create the second project: %v", err)
		}
		links := len(w.linkRows(t, w.refID))
		got := w.attach(t, w.member, w.attachArgs("routes_to", "project", second.ID))
		wantWorkRefusal(t, "a second route", got, 409, "request.conflict")
		if !strings.Contains(got.errMessage, fmt.Sprint(w.project)) {
			t.Errorf("the conflict does not name the current project %d: %q", w.project, got.errMessage)
		}
		if n := len(w.linkRows(t, w.refID)); n != links {
			t.Fatalf("%d links after a refused route, want %d", n, links)
		}

		// The deliberate move appends the new routing and keeps the old row: a
		// request's history is not rewritten by moving it.
		args := w.attachArgs("routes_to", "project", second.ID)
		args["supersede_route"] = true
		got = w.attach(t, w.member, args)
		if got.errMessage != "" {
			t.Fatalf("superseding refused: %s %v", got.errMessage, got.errData)
		}
		if changed, _ := got.result["changed"].(bool); !changed {
			t.Fatalf("the supersede reported changed=false: %v", got.result)
		}
		if n := len(w.linkRows(t, w.refID)); n != links+1 {
			t.Fatalf("%d links after a supersede, want %d (the old routing is kept)", n, links+1)
		}
		// Move it back so the rest of this test plans into the fixture's project.
		args = w.attachArgs("routes_to", "project", w.project)
		args["supersede_route"] = true
		if got := w.attach(t, w.member, args); got.errMessage != "" {
			t.Fatalf("moving the route back refused: %s %v", got.errMessage, got.errData)
		}
	})

	t.Run("a plan item links by story", func(t *testing.T) {
		got := w.attach(t, w.member, w.attachArgs("planned_as", "story", w.story))
		if got.errMessage != "" {
			t.Fatalf("planned_as refused: %s %v", got.errMessage, got.errData)
		}
		if changed, _ := got.result["changed"].(bool); !changed {
			t.Fatalf("the first planned_as reported changed=false: %v", got.result)
		}
	})

	t.Run("a pair the contract never allows is an invalid request", func(t *testing.T) {
		got := w.attach(t, w.member, w.attachArgs("planned_as", "artifact", 1))
		wantWorkRefusal(t, "planned_as -> artifact", got, 400, "request.invalid")
		if !strings.Contains(got.errMessage, "planned_as") {
			t.Errorf("the refusal does not name the kind: %q", got.errMessage)
		}
	})

	t.Run("a duplicate fold is refused by name", func(t *testing.T) {
		got := w.attach(t, w.member, w.attachArgs("duplicate_of", "request", w.refID))
		wantWorkRefusal(t, "duplicate_of", got, 400, "request.invalid")
		if !strings.Contains(got.errMessage, "duplicate") {
			t.Errorf("the refusal does not explain the fold: %q", got.errMessage)
		}
	})

	t.Run("a target in the same org the caller cannot see is not there", func(t *testing.T) {
		links := len(w.linkRows(t, w.refID))
		got := w.attach(t, w.member, w.attachArgs("planned_as", "task", w.hiddenTask))
		// The service answers for the target it named, so the refusal names no
		// project id the caller had no right to learn.
		wantWorkRefusal(t, "a hidden project's ticket", got, 404, "")
		if !strings.Contains(got.errMessage, fmt.Sprint(w.hiddenTask)) {
			t.Errorf("the refusal does not name the target: %q", got.errMessage)
		}
		if n := len(w.linkRows(t, w.refID)); n != links {
			t.Fatalf("%d links after a refused target, want %d", n, links)
		}
	})

	t.Run("a target in another org is the same 404 as an unknown target", func(t *testing.T) {
		foreign := w.attach(t, w.member, w.attachArgs("delivered_by", "task", w.foreignTask))
		unknown := w.attach(t, w.member, w.attachArgs("delivered_by", "task", 999_999_999))
		wantWorkRefusal(t, "another org's ticket", foreign, 404, "")
		wantWorkRefusal(t, "an unknown ticket", unknown, 404, "")
	})

	t.Run("a stranger learns nothing", func(t *testing.T) {
		got := w.attach(t, w.other, w.attachArgs("routes_to", "project", w.project))
		wantWorkRefusal(t, "a customer of another org", got, 404, "request.not_found")
	})

	t.Run("the requester plans nothing", func(t *testing.T) {
		// The requester here is a customer with a submit grant and no team role;
		// over MCP they are not a UI principal, so the service's capability answer
		// is "no standing" — the surface's one 404, which is what the reviewed
		// capability rule gives a non-UI caller. Either way they plan nothing, and
		// that is what this asserts: no row is written.
		links := len(w.linkRows(t, w.refID))
		got := w.attach(t, w.customer, w.attachArgs("planned_as", "story", w.story))
		if got.errMessage == "" {
			t.Fatalf("the requester attached a plan item: %v", got.result)
		}
		if n := len(w.linkRows(t, w.refID)); n != links {
			t.Fatalf("%d links after the requester's refused attach, want %d", n, links)
		}
	})

	t.Run("the submit-only credential reaches none of it", func(t *testing.T) {
		scoped := w.scoped(t, w.member, w.team)
		got := w.attach(t, scoped, w.attachArgs("planned_as", "story", w.story))
		if !strings.Contains(got.errMessage, "may only submit") {
			t.Fatalf("a scoped credential's attach = %q, want the scope refusal", got.errMessage)
		}
	})
}

// ---- criterion 1b: a retry-safe handoff that approves nothing ----

// TestMCPRequestPlanning_HandoffIsRetrySafe drives a batch through the tool: two
// tickets with a dependency, the untrusted quote in each description, approval
// and assignment untouched — and then the same key again, which must plan no
// second ticket.
func TestMCPRequestPlanning_HandoffIsRetrySafe(t *testing.T) {
	w := newPlanningWorld(t)
	if got := w.attach(t, w.member, w.attachArgs("routes_to", "project", w.project)); got.errMessage != "" {
		t.Fatalf("route refused: %s %v", got.errMessage, got.errData)
	}

	items := []interface{}{
		map[string]interface{}{
			"ref": "api", "story_id": float64(w.story), "name": "Add the export endpoint",
			"plan": "Build the CSV export the request asks for.", "acceptance_criteria": "a CSV downloads",
			"priority": "2",
		},
		map[string]interface{}{
			"ref": "docs", "story_id": float64(w.story), "name": "Document the export",
			"plan": "Write the operator page.", "acceptance_criteria": "docs published",
			"depends_on": []interface{}{"api"},
		},
	}
	args := map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(w.refID),
		"handoff_key": "batch-one", "items": items,
	}
	before := w.countRows(t, `SELECT count(*) FROM tasks WHERE project_id = $1`, w.project)

	got := w.tool(t, w.member, "amp_handoff_request", args)
	if got.errMessage != "" {
		t.Fatalf("handoff refused: %s %v", got.errMessage, got.errData)
	}
	if n := intField(t, got.result, "created"); n != 2 {
		t.Fatalf("created = %d, want 2 (%v)", n, got.result)
	}
	if intField(t, got.result, "project_id") != w.project {
		t.Fatalf("planned into project %v, want the routed %d", got.result["project_id"], w.project)
	}
	tasks := handoffItems(t, got.result)
	if tasks["api"] == 0 || tasks["docs"] == 0 {
		t.Fatalf("items = %v", got.result)
	}
	if n := w.countRows(t, `SELECT count(*) FROM tasks WHERE project_id = $1`, w.project); n != before+2 {
		t.Fatalf("%d tickets after the handoff, want %d", n, before+2)
	}

	api, docs := w.task(t, tasks["api"]), w.task(t, tasks["docs"])
	if len(docs.DependencyIDs) != 1 || docs.DependencyIDs[0] != api.ID {
		t.Fatalf("docs' dependencies = %v, want [%d]", docs.DependencyIDs, api.ID)
	}
	for _, tk := range []taskRow{api, docs} {
		// The description is the team's plan, then the requester's words quoted as
		// untrusted data — the fence is the domain's, so this asserts the tool did
		// not bypass it.
		if !strings.Contains(tk.Description, domain.UntrustedBegin) || !strings.Contains(tk.Description, domain.UntrustedEnd) {
			t.Errorf("ticket %d's description carries no untrusted fence: %q", tk.ID, tk.Description)
		}
		if !strings.Contains(tk.Description, strings.TrimSpace(w.stored(t, w.ref).Subject)) {
			t.Errorf("ticket %d's description does not quote the request", tk.ID)
		}
		// Nothing decision-shaped came from the payload or the request.
		if tk.ApprovedAt != nil && *tk.ApprovedAt != "" {
			t.Errorf("ticket %d is approved (%q): a handoff approves nothing", tk.ID, *tk.ApprovedAt)
		}
		if tk.StartAt != nil && *tk.StartAt != "" {
			t.Errorf("ticket %d is scheduled (%q): a handoff schedules nothing", tk.ID, *tk.StartAt)
		}
		if tk.AssignedTo != nil && *tk.AssignedTo != "" {
			t.Errorf("ticket %d is assigned to an agent (%q): a handoff assigns nothing", tk.ID, *tk.AssignedTo)
		}
		// The owner is the triager who planned it — the plan author, never the
		// requester.
		if tk.OwnerUserID == nil || *tk.OwnerUserID != w.member.User.ID {
			t.Errorf("ticket %d's owner = %v, want the planning triager %d", tk.ID, tk.OwnerUserID, w.member.User.ID)
		}
		// And the ledger marker rides the ticket, which is what makes the retry
		// reconstruct the batch.
		if !strings.HasPrefix(tk.MetadataHandof, fmt.Sprintf("%d/batch-one/", w.refID)) {
			t.Errorf("ticket %d's handoff marker = %q", tk.ID, tk.MetadataHandof)
		}
	}

	t.Run("the same key plans no second ticket", func(t *testing.T) {
		tasksBefore := w.countRows(t, `SELECT count(*) FROM tasks WHERE project_id = $1`, w.project)
		linksBefore := len(w.linkRows(t, w.refID))
		auditsBefore := w.auditCount(t, "request.handoff", w.refID)

		replay := w.tool(t, w.member, "amp_handoff_request", args)
		if replay.errMessage != "" {
			t.Fatalf("the replay was refused: %s %v", replay.errMessage, replay.errData)
		}
		if n := intField(t, replay.result, "created"); n != 0 {
			t.Fatalf("a replay created %d tickets, want 0 (%v)", n, replay.result)
		}
		if n := intField(t, replay.result, "already"); n != 2 {
			t.Fatalf("a replay reports %d already-there items, want 2 (%v)", n, replay.result)
		}
		if n := w.countRows(t, `SELECT count(*) FROM tasks WHERE project_id = $1`, w.project); n != tasksBefore {
			t.Fatalf("%d tickets after the replay, want %d", n, tasksBefore)
		}
		if n := len(w.linkRows(t, w.refID)); n != linksBefore {
			t.Fatalf("%d links after the replay, want %d", n, linksBefore)
		}
		// A fully-finished replay writes nothing at all — no audit row either.
		if n := w.auditCount(t, "request.handoff", w.refID); n != auditsBefore {
			t.Fatalf("%d handoff audit rows after the replay, want %d", n, auditsBefore)
		}
		// And the refs still resolve to the same tickets.
		if again := handoffItems(t, replay.result); again["api"] != tasks["api"] || again["docs"] != tasks["docs"] {
			t.Fatalf("the replay resolved %v, want %v", again, tasks)
		}
	})

	t.Run("a new key is a deliberate second plan", func(t *testing.T) {
		tasksBefore := w.countRows(t, `SELECT count(*) FROM tasks WHERE project_id = $1`, w.project)
		second := map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(w.refID), "handoff_key": "batch-two",
			"items": []interface{}{map[string]interface{}{
				"ref": "api", "story_id": float64(w.story), "name": "Add the export endpoint",
			}},
		}
		out := w.tool(t, w.member, "amp_handoff_request", second)
		if out.errMessage != "" {
			t.Fatalf("a new key was refused: %s %v", out.errMessage, out.errData)
		}
		if n := w.countRows(t, `SELECT count(*) FROM tasks WHERE project_id = $1`, w.project); n != tasksBefore+1 {
			t.Fatalf("%d tickets after a re-plan, want %d", n, tasksBefore+1)
		}
	})

	t.Run("an unrouted request says to route it first", func(t *testing.T) {
		// A second request in the same inbox, routed nowhere.
		sub := w.callTool(t, w.member, mcp.SubmitScopeTool, submitArgs(w.team, "unrouted "+uniq("h"), ""))
		if sub.errMessage != "" {
			t.Fatalf("fixture submission refused: %s %v", sub.errMessage, sub.errData)
		}
		unrouted := w.stored(t, requestField(t, sub, "public_ref").(string)).ID
		got := w.tool(t, w.member, "amp_handoff_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(unrouted), "handoff_key": "k",
			"items": []interface{}{map[string]interface{}{"ref": "a", "story_id": float64(w.story), "name": "x"}},
		})
		wantWorkRefusal(t, "handoff of an unrouted request", got, 400, "request.invalid")
		if !strings.Contains(got.errMessage, "route") {
			t.Errorf("the refusal does not say to route the request: %q", got.errMessage)
		}
	})

	t.Run("the payload cannot decide who, when or whether approved", func(t *testing.T) {
		tasksBefore := w.countRows(t, `SELECT count(*) FROM tasks WHERE project_id = $1`, w.project)
		for _, c := range []struct {
			name string
			args map[string]interface{}
		}{
			{"project_id at the top level", map[string]interface{}{
				"team_id": float64(w.team), "request_id": float64(w.refID), "handoff_key": "decide",
				"project_id": float64(w.hiddenProj),
				"items":      []interface{}{map[string]interface{}{"ref": "a", "story_id": float64(w.story), "name": "x"}},
			}},
			{"assigned_to on an item", map[string]interface{}{
				"team_id": float64(w.team), "request_id": float64(w.refID), "handoff_key": "decide",
				"items": []interface{}{map[string]interface{}{"ref": "a", "story_id": float64(w.story), "name": "x",
					"assigned_to": "amp-worker"}},
			}},
			{"start_at on an item", map[string]interface{}{
				"team_id": float64(w.team), "request_id": float64(w.refID), "handoff_key": "decide",
				"items": []interface{}{map[string]interface{}{"ref": "a", "story_id": float64(w.story), "name": "x",
					"start_at": "2027-01-01T00:00:00Z"}},
			}},
			{"approve on an item", map[string]interface{}{
				"team_id": float64(w.team), "request_id": float64(w.refID), "handoff_key": "decide",
				"items": []interface{}{map[string]interface{}{"ref": "a", "story_id": float64(w.story), "name": "x",
					"approve": true}},
			}},
		} {
			got := w.tool(t, w.member, "amp_handoff_request", c.args)
			if got.errMessage == "" {
				t.Fatalf("%s was accepted: %v", c.name, got.result)
			}
			if got.errData["code"] != "request.invalid" {
				t.Errorf("%s code = %v, want request.invalid (%q)", c.name, got.errData["code"], got.errMessage)
			}
		}
		if n := w.countRows(t, `SELECT count(*) FROM tasks WHERE project_id = $1`, w.project); n != tasksBefore {
			t.Fatalf("%d tickets after the refused payloads, want %d", n, tasksBefore)
		}
	})

	t.Run("a batch over the bound is refused before anything is created", func(t *testing.T) {
		tasksBefore := w.countRows(t, `SELECT count(*) FROM tasks WHERE project_id = $1`, w.project)
		items := make([]interface{}, 101)
		for i := range items {
			items[i] = map[string]interface{}{"ref": fmt.Sprintf("r%d", i), "story_id": float64(w.story), "name": "x"}
		}
		got := w.tool(t, w.member, "amp_handoff_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(w.refID), "handoff_key": "too-many", "items": items,
		})
		wantWorkRefusal(t, "a 101-item batch", got, 400, "request.invalid")
		if n := w.countRows(t, `SELECT count(*) FROM tasks WHERE project_id = $1`, w.project); n != tasksBefore {
			t.Fatalf("%d tickets after the refused batch, want %d", n, tasksBefore)
		}
	})

	t.Run("the requester plans nothing", func(t *testing.T) {
		got := w.tool(t, w.customer, "amp_handoff_request", map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(w.refID), "handoff_key": "req",
			"items": []interface{}{map[string]interface{}{"ref": "a", "story_id": float64(w.story), "name": "x"}},
		})
		if got.errMessage == "" {
			t.Fatalf("the requester planned work: %v", got.result)
		}
	})
}

// ---- criterion 2: bounded uploads and no fetch ----

// TestMCPRequestPlanning_UploadIsBoundedAndFetchesNothing: the bytes travel in the
// call, the type is the sniffer's, the per-file and per-request bounds are
// enforced before anything is stored, and an argument that names a location is
// refused rather than ignored.
func TestMCPRequestPlanning_UploadIsBoundedAndFetchesNothing(t *testing.T) {
	w := newPlanningWorld(t)
	upload := func(t *testing.T, who *auth.Identity, args map[string]interface{}) mcpCallResult {
		t.Helper()
		return w.tool(t, who, "amp_upload_request_file", args)
	}
	// The request is the member's own submission, so the member is its requester
	// as well as a triager — the same person on both rows, which is the case the
	// capability rule has to get right for the requester's own uploads too.
	notes := []byte("the requester's private notes: do not leak these\n")
	args := func(name string, content []byte) map[string]interface{} {
		return map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(w.refID), "file_name": name,
			"content_base64": base64.StdEncoding.EncodeToString(content),
		}
	}
	stored := func() int {
		return w.countRows(t, `SELECT count(*) FROM request_attachments WHERE request_id = $1`, w.refID)
	}

	t.Run("stores the bytes with the sniffed type", func(t *testing.T) {
		// A name that says an image, bytes that are text: the name never decides
		// the type.
		got := upload(t, w.member, args("screenshot.png", notes))
		if got.errMessage != "" {
			t.Fatalf("upload refused: %s %v", got.errMessage, got.errData)
		}
		att, _ := got.result["attachment"].(map[string]interface{})
		if att == nil {
			t.Fatalf("no attachment in the answer: %v", got.result)
		}
		if att["content_type"] != "text/plain" {
			t.Errorf("stored content_type = %v, want the sniffed text/plain", att["content_type"])
		}
		if intField(t, att, "size_bytes") != len(notes) {
			t.Errorf("size_bytes = %v, want %d", att["size_bytes"], len(notes))
		}
		if att["scan_state"] != string(domain.RequestScanUnsupported) {
			t.Errorf("scan_state = %v, want %q (no scanner ran)", att["scan_state"], domain.RequestScanUnsupported)
		}
		if held, _ := att["keep"].(bool); held {
			t.Errorf("the upload pinned the file (keep=true): an upload is not a retention decision")
		}
		if !strings.Contains(strField(t, got.result, "next"), "private") {
			t.Errorf("the answer does not describe the file as private: %q", strField(t, got.result, "next"))
		}
	})

	t.Run("the requester may upload to their own request", func(t *testing.T) {
		// A request the customer submitted, and the customer's own upload to it.
		sub := w.callTool(t, w.customer, mcp.SubmitScopeTool, submitArgs(w.team, "customer file "+uniq("u"), ""))
		if sub.errMessage != "" {
			t.Fatalf("fixture submission refused: %s %v", sub.errMessage, sub.errData)
		}
		custRef := w.stored(t, requestField(t, sub, "public_ref").(string)).ID
		got := upload(t, w.customer, map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(custRef), "file_name": "notes.txt",
			"content_base64": base64.StdEncoding.EncodeToString(notes),
		})
		if got.errMessage != "" {
			t.Fatalf("the requester's own upload was refused: %s %v", got.errMessage, got.errData)
		}
	})

	t.Run("an archive is stored and held", func(t *testing.T) {
		// A zip's signature, which is what the sniffer reads; the name does not
		// matter and the file is HELD for a team lead rather than refused.
		zip := []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00")
		got := upload(t, w.member, args("bundle.zip", zip))
		if got.errMessage != "" {
			t.Fatalf("an archive was refused: %s %v", got.errMessage, got.errData)
		}
		if next := strField(t, got.result, "next"); !strings.Contains(next, "HELD") {
			t.Errorf("the answer does not say the file is held: %q", next)
		}
	})

	t.Run("a URL instead of bytes is refused, not ignored", func(t *testing.T) {
		before := stored()
		for _, key := range []string{"url", "content_url", "path", "fetch_url"} {
			payload := map[string]interface{}{
				"team_id": float64(w.team), "request_id": float64(w.refID), "file_name": "notes.txt",
				"content_base64": base64.StdEncoding.EncodeToString(notes), key: "https://example.com/a.txt",
			}
			got := upload(t, w.member, payload)
			wantWorkRefusal(t, "a payload naming "+key, got, 400, "request.invalid")
			if !strings.Contains(got.errMessage, "never fetches") {
				t.Errorf("%q refusal does not say AMP never fetches: %q", key, got.errMessage)
			}
		}
		if n := stored(); n != before {
			t.Fatalf("%d attachments after the refused payloads, want %d", n, before)
		}
	})

	t.Run("an over-size file is refused before it is stored", func(t *testing.T) {
		before := stored()
		big := make([]byte, domain.MaxRequestAttachmentBytes+1)
		got := upload(t, w.member, args("big.bin", big))
		wantWorkRefusal(t, "a file over the per-file bound", got, 400, "request.invalid")
		if !strings.Contains(got.errMessage, "per-file limit") {
			t.Errorf("the refusal does not name the bound: %q", got.errMessage)
		}
		if n := stored(); n != before {
			t.Fatalf("%d attachments after an over-size upload, want %d", n, before)
		}
	})

	t.Run("the sixth file is refused", func(t *testing.T) {
		// The fixture already has two files on this request; fill it to the bound
		// and then try one more. The bound is the domain's (MaxRequestAttachments).
		for stored() < domain.MaxRequestAttachments {
			got := upload(t, w.member, args(fmt.Sprintf("note-%d.txt", stored()), notes))
			if got.errMessage != "" {
				t.Fatalf("filling the request was refused at %d files: %s %v", stored(), got.errMessage, got.errData)
			}
		}
		got := upload(t, w.member, args("one-too-many.txt", notes))
		wantWorkRefusal(t, "a sixth file", got, 400, "request.invalid")
		if n := stored(); n != domain.MaxRequestAttachments {
			t.Fatalf("%d attachments after the refused sixth, want %d", n, domain.MaxRequestAttachments)
		}
	})

	t.Run("a stranger learns nothing", func(t *testing.T) {
		before := stored()
		got := upload(t, w.other, args("notes.txt", notes))
		wantWorkRefusal(t, "a customer of another org", got, 404, "request.not_found")
		if n := stored(); n != before {
			t.Fatalf("%d attachments after a stranger's upload, want %d", n, before)
		}
	})

	t.Run("naming another team is not there", func(t *testing.T) {
		before := stored()
		payload := args("notes.txt", notes)
		payload["team_id"] = float64(w.otherTeam)
		got := upload(t, w.member, payload)
		wantWorkRefusal(t, "the request named through another team", got, 404, "request.not_found")
		if n := stored(); n != before {
			t.Fatalf("%d attachments after a wrong-team upload, want %d", n, before)
		}
	})

	t.Run("an unreadable payload is refused and never echoed", func(t *testing.T) {
		got := upload(t, w.member, map[string]interface{}{
			"team_id": float64(w.team), "request_id": float64(w.refID), "file_name": "x.txt",
			"content_base64": "!!not base64!!SECRET",
		})
		wantWorkRefusal(t, "an unreadable payload", got, 400, "request.invalid")
		if strings.Contains(got.errMessage, "SECRET") {
			t.Errorf("the refusal echoes the content: %q", got.errMessage)
		}
	})

	t.Run("the submit-only credential cannot upload", func(t *testing.T) {
		scoped := w.scoped(t, w.member, w.team)
		got := upload(t, scoped, args("notes.txt", notes))
		if !strings.Contains(got.errMessage, "may only submit") {
			t.Fatalf("a scoped credential's upload = %q, want the scope refusal", got.errMessage)
		}
	})
}

// ---- criterion 3: no acceptance, no auto-approval ----

// TestMCPRequestPlanning_NoAcceptanceAndNoAutoApproval is the negative boundary as
// a property of the surface: no tool in the request surface accepts an outcome for
// a requester or delivers anything; a handoff's tickets are all still awaiting a
// person's approval and the real dispatch gate refuses them; and nothing on this
// surface moved the request's own lifecycle state.
func TestMCPRequestPlanning_NoAcceptanceAndNoAutoApproval(t *testing.T) {
	w := newPlanningWorld(t)

	// The whole surface refuses an acceptance-shaped call as an absent tool — not
	// as a refusal that would imply the capability exists and was denied.
	for _, name := range []string{"amp_accept_request", "amp_deliver_request"} {
		got := w.tool(t, w.member, name, map[string]interface{}{"team_id": float64(w.team), "request_id": float64(w.refID)})
		if !strings.Contains(got.errMessage, "not found") {
			t.Fatalf("%s answered %q; there is no such tool", name, got.errMessage)
		}
	}

	// No listed request tool offers those verbs either.
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	b, err := json.Marshal(w.s.srv.HandleMessage(w.s.callCtx(), raw))
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	for _, tl := range list.Result.Tools {
		if !strings.Contains(tl.Name, "request") {
			continue
		}
		for _, forbidden := range []string{"accept", "deliver", "transition", "reopen", "answer", "reject", "approve"} {
			if strings.Contains(tl.Name, forbidden) {
				t.Fatalf("the request surface has %q, which belongs to the requester's or the team's own web-UI act", tl.Name)
			}
		}
	}

	// Do a full round of real work through the surface: link, plan, upload.
	if got := w.attach(t, w.member, w.attachArgs("routes_to", "project", w.project)); got.errMessage != "" {
		t.Fatalf("route refused: %s %v", got.errMessage, got.errData)
	}
	if got := w.attach(t, w.member, w.attachArgs("planned_as", "story", w.story)); got.errMessage != "" {
		t.Fatalf("planned_as refused: %s %v", got.errMessage, got.errData)
	}
	planned := w.tool(t, w.member, "amp_handoff_request", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(w.refID), "handoff_key": "boundary",
		"items": []interface{}{map[string]interface{}{"ref": "a", "story_id": float64(w.story), "name": "Do the work"}},
	})
	if planned.errMessage != "" {
		t.Fatalf("handoff refused: %s %v", planned.errMessage, planned.errData)
	}
	taskID := handoffItems(t, planned.result)["a"]
	if taskID == 0 {
		t.Fatalf("the handoff created no ticket: %v", planned.result)
	}
	if got := w.tool(t, w.member, "amp_upload_request_file", map[string]interface{}{
		"team_id": float64(w.team), "request_id": float64(w.refID), "file_name": "notes.txt",
		"content_base64": base64.StdEncoding.EncodeToString([]byte("hello\n")),
	}); got.errMessage != "" {
		t.Fatalf("upload refused: %s %v", got.errMessage, got.errData)
	}

	// The request's state is untouched by every one of those acts: linking,
	// planning and uploading are booking, never the lifecycle (that is
	// Service.TransitionRequest's, and it has no tool here).
	if state := w.stateOf(t, w.refID); state != string(domain.RequestStatusNew) {
		t.Fatalf("state = %q after linking, planning and uploading, want new", state)
	}
	// And no delivery or acceptance column was written.
	if n := w.countRows(t, `SELECT count(*) FROM team_requests WHERE id = $1 AND (delivered_at IS NOT NULL
	         OR accepted_at IS NOT NULL OR accepted_by IS NOT NULL OR outcome_brief <> '')`, w.refID); n != 0 {
		t.Fatalf("%d request(s) carry delivery or acceptance state: no tool here can set an outcome", n)
	}

	// The created ticket is unapproved, and the REAL dispatch gate refuses it —
	// a handoff plans work for a person to approve, it never starts it.
	tk := w.task(t, taskID)
	if tk.ApprovedAt != nil && *tk.ApprovedAt != "" {
		t.Fatalf("the planned ticket is already approved (%q)", *tk.ApprovedAt)
	}
	dispatch := w.tool(t, w.member, "amp_dispatch_task", map[string]interface{}{
		"task_id": float64(taskID), "agent_id": "amp-worker",
	})
	if dispatch.errMessage == "" {
		t.Fatalf("the planned ticket dispatched without approval: %v", dispatch.result)
	}
	if !strings.Contains(dispatch.errMessage, "approved") {
		t.Errorf("the dispatch refusal does not name the approval gate: %q", dispatch.errMessage)
	}
	// Nothing was dispatched by any act above either.
	if n := w.countRows(t, `SELECT count(*) FROM tasks WHERE id = $1 AND (dispatch_id <> '' OR dispatched_at IS NOT NULL
	         OR started_at IS NOT NULL)`, taskID); n != 0 {
		t.Fatalf("ticket %d carries dispatch state: a handoff dispatches nothing", taskID)
	}

	// The relation and the file are real, so this negative is proven against a
	// surface that did work rather than one that did nothing.
	var links, attachments int
	if err := w.db(t).QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM request_links WHERE request_id = $1),
		        (SELECT count(*) FROM request_attachments WHERE request_id = $1)`, w.refID).Scan(&links, &attachments); err != nil {
		t.Fatal(err)
	}
	if links < 2 || attachments < 1 {
		t.Fatalf("links=%d attachments=%d: the boundary test did not exercise the surface", links, attachments)
	}
}
