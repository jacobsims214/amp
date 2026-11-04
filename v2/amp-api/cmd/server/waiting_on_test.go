package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
)

// Waiting on you (AMP #2478, KB decisions/human-work-ux.md §1 and §3):
// WaitingOn's four kinds and coming_up over a seeded project, served by the
// project's My work tab, the top-level summary and the badge counts.

type waitFixture struct {
	w  *rbacWorld
	ra *restAs
	// alice's tickets in the shared project
	build, deploy, solo, q1, q2, doReady, doStarted, comingDeps, comingApproval, bobsHuman, epic2Task int
	epic2                                                                                           int
	// a second project with one question for alice
	proj2, q3 int
}

func approveAs(t *testing.T, ra *restAs, who string, projectID int, ids ...int) {
	t.Helper()
	list := make([]any, len(ids))
	for i, id := range ids {
		list[i] = float64(id)
	}
	res := ra.want(t, 200, who, http.MethodPost, fmt.Sprintf("/api/projects/%d/approve-plan", projectID), map[string]any{"task_ids": list})
	if got := fmt.Sprint(res["approved"]); len(res["approved"].([]any)) != len(ids) {
		t.Fatalf("approve %v approved %s (skipped %v)", ids, got, res["skipped"])
	}
}

func mkHuman(t *testing.T, s *mcpStack, storyID int, name, owner string, deps ...int) int {
	t.Helper()
	ids := make([]any, len(deps))
	for i, d := range deps {
		ids[i] = fmt.Sprintf("#%d", d)
	}
	res := s.call(t, "amp_create_tasks", map[string]any{"project_id": float64(s.projectID), "tasks": []any{
		map[string]any{"ref": "h", "story_id": float64(storyID), "name": name, "description": "d", "acceptance_criteria": "a",
			"performer": "human", "human_reason": "review", "owner": owner, "depends_on": ids},
	}})
	return int(res["created"].([]any)[0].(map[string]any)["id"].(float64))
}

func newWaitFixture(t *testing.T) *waitFixture {
	t.Helper()
	ctx := context.Background()
	w := newRBACWorld(t)
	f := &waitFixture{w: w, ra: newRestAs(t, w)}
	on := true
	if _, err := w.s.repo.UpdateProject(ctx, w.s.projectID, domain.ProjectSettings{RequirePlanApproval: &on}); err != nil {
		t.Fatal(err)
	}
	alice := w.s.as(w.alice)
	pid, story := w.s.projectID, w.s.storyID

	// Merge: build completes with a PR (merge needed); deploy waits on it.
	f.build = mkIn(t, alice, story, "build")
	f.deploy = mkIn(t, alice, story, "deploy", f.build)
	f.solo = mkIn(t, alice, story, "solo")
	approveAs(t, f.ra, "alice", pid, f.build)
	finishWith(t, alice, f.build, map[string]any{"pr_url": "https://git.example/pr/" + uniq("w"), "pushed": true})

	// Answer: two questions, q1 asked first.
	f.q1 = mkIn(t, alice, story, "q1")
	f.q2 = mkIn(t, alice, story, "q2")
	approveAs(t, f.ra, "alice", pid, f.q1, f.q2)
	alice.call(t, "amp_block_task", map[string]any{"task_id": float64(f.q1), "reason": "Which provider?"})
	alice.call(t, "amp_block_task", map[string]any{"task_id": float64(f.q2), "reason": "Which region?"})

	// Do: one ready, one started.
	f.doReady = mkHuman(t, alice, story, "sign off", w.alice.Email)
	f.doStarted = mkHuman(t, alice, story, "call the vendor", w.alice.Email)
	approveAs(t, f.ra, "alice", pid, f.doReady, f.doStarted)
	f.ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/start", f.doStarted), nil)

	// Coming up: one waiting on deploy, one waiting on approval.
	f.comingDeps = mkHuman(t, alice, story, "after deploy", w.alice.Email, f.deploy)
	approveAs(t, f.ra, "alice", pid, f.comingDeps)
	f.comingApproval = mkHuman(t, alice, story, "not approved yet", w.alice.Email)

	// Bob's human ticket, created by alice: bob's, not alice's.
	f.bobsHuman = mkHuman(t, alice, story, "bob reviews", w.bob.Email)
	approveAs(t, f.ra, "bob", pid, f.bobsHuman)

	// A second epic with one ticket awaiting alice's approval.
	epicID := intField(t, alice.call(t, "amp_create_epic", map[string]any{"project_id": float64(pid), "name": "second epic"}), "id")
	storyID := intField(t, alice.call(t, "amp_create_story", map[string]any{"project_id": float64(pid), "epic_id": float64(epicID),
		"name": "second story", "acceptance_criteria": "done"}), "id")
	f.epic2 = epicID
	f.epic2Task = mkIn(t, alice, storyID, "epic two work")

	// A second project (alice is on devs) with one question.
	p2, err := w.s.repo.CreateProject(ctx, domain.CreateProjectRequest{Name: "zz second " + uniq("p"), Code: uniq("p2"), OrgID: &w.org.ID, TeamID: &w.devs.ID})
	if err != nil {
		t.Fatal(err)
	}
	planApprovalOff(t, w.s.repo, p2.ID)
	e2, err := w.s.repo.CreateEpic(ctx, domain.CreateEpicRequest{ProjectID: p2.ID, Name: "p2 epic", Priority: "1"})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := w.s.repo.CreateStory(ctx, domain.CreateStoryRequest{ProjectID: p2.ID, EpicID: e2.ID, Name: "p2 story", Priority: "1"})
	if err != nil {
		t.Fatal(err)
	}
	other := *alice
	other.projectID, other.epicID, other.storyID = p2.ID, e2.ID, s2.ID
	f.proj2 = p2.ID
	f.q3 = mkIn(t, &other, s2.ID, "q3")
	other.call(t, "amp_block_task", map[string]any{"task_id": float64(f.q3), "reason": "Ship it?"})
	return f
}

func itemTaskIDs(v any) []int {
	var out []int
	for _, x := range v.([]any) {
		m := x.(map[string]any)
		if id, ok := m["task_id"].(float64); ok {
			out = append(out, int(id))
		} else {
			out = append(out, int(m["epic_id"].(float64)))
		}
	}
	return out
}

func countsOf(v any) map[string]int {
	out := map[string]int{}
	for k, x := range v.(map[string]any) {
		out[k] = int(x.(float64))
	}
	return out
}

func TestWaitingOn_ProjectTabKindsOrderAndPlan(t *testing.T) {
	f := newWaitFixture(t)
	pid := f.w.s.projectID
	out := f.ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work", pid), nil)
	items := out["items"].(map[string]any)

	// approve: one per epic, oldest first; the default epic holds deploy,
	// solo and the human ticket awaiting approval (alice's own only).
	appr := items["approve"].([]any)
	if len(appr) != 2 {
		t.Fatalf("approve items = %v", appr)
	}
	first := appr[0].(map[string]any)
	if int(first["epic_id"].(float64)) != f.w.s.epicID || int(appr[1].(map[string]any)["epic_id"].(float64)) != f.epic2 {
		t.Fatalf("approve order = %v", itemTaskIDs(appr))
	}
	got := []int{}
	for _, x := range first["task_ids"].([]any) {
		got = append(got, int(x.(float64)))
	}
	slices.Sort(got)
	want := []int{f.deploy, f.solo, f.comingApproval}
	slices.Sort(want)
	if !slices.Equal(got, want) || int(first["count"].(float64)) != 3 {
		t.Fatalf("approve task_ids = %v, want %v", got, want)
	}

	// answer: q1 before q2.
	if ids := itemTaskIDs(items["answer"]); !slices.Equal(ids, []int{f.q1, f.q2}) {
		t.Fatalf("answer = %v, want [%d %d]", ids, f.q1, f.q2)
	}
	if q := items["answer"].([]any)[0].(map[string]any)["question"]; q != "Which provider?" {
		t.Fatalf("question = %v", q)
	}
	// merge: build, with its PR and what it holds up.
	merge := items["merge"].([]any)
	if ids := itemTaskIDs(merge); !slices.Equal(ids, []int{f.build}) {
		t.Fatalf("merge = %v", ids)
	}
	m := merge[0].(map[string]any)
	if m["merge_state"] != domain.MergeNeeded || m["pr_url"] == nil || fmt.Sprint(m["holding_up"]) != fmt.Sprintf("[%d]", f.deploy) {
		t.Fatalf("merge item = %v", m)
	}
	// do: alice's ready and started human tickets; never bob's.
	do := itemTaskIDs(items["do"])
	if !slices.Contains(do, f.doReady) || !slices.Contains(do, f.doStarted) || slices.Contains(do, f.bobsHuman) || len(do) != 2 {
		t.Fatalf("do = %v", do)
	}
	// coming_up: waiting on deploy, and waiting on approval.
	cu := out["coming_up"].([]any)
	if ids := itemTaskIDs(cu); !slices.Equal(ids, []int{f.comingDeps, f.comingApproval}) {
		t.Fatalf("coming_up = %v", ids)
	}
	if ws := cu[0].(map[string]any)["waits_on"].([]any); len(ws) != 1 || int(ws[0].(map[string]any)["id"].(float64)) != f.deploy {
		t.Fatalf("coming_up waits_on = %v", ws)
	}
	if cu[1].(map[string]any)["awaiting_approval"] != true {
		t.Fatalf("coming_up awaiting_approval = %v", cu[1])
	}
	c := countsOf(out["counts"])
	if c["approve"] != 2 || c["answer"] != 2 || c["merge"] != 1 || c["do"] != 2 || c["coming_up"] != 2 || c["waiting"] != 7 {
		t.Fatalf("counts = %v", c)
	}
	if out["read_only"] != false {
		t.Fatalf("own list read_only = %v", out["read_only"])
	}

	// The plan in place.
	plan := first["plan"].(map[string]any)
	tickets := map[int]map[string]any{}
	for _, x := range plan["tickets"].([]any) {
		pt := x.(map[string]any)
		tickets[int(pt["id"].(float64))] = pt
	}
	d := tickets[f.deploy]
	if d == nil || d["awaiting"] != true || d["yours"] != true || d["change"] != "new" || d["description"] != "d" || d["acceptance_criteria"] != "a" {
		t.Fatalf("deploy in plan = %v", d)
	}
	if fmt.Sprint(d["waits_on"]) != fmt.Sprintf("[%d]", f.build) || d["starts_on_approval"] != false {
		t.Fatalf("deploy waits on its unmerged build: %v", d)
	}
	if tickets[f.q1] == nil || tickets[f.q1]["awaiting"] != false || tickets[f.q1]["change"] != nil {
		t.Fatalf("approved ticket in plan = %v", tickets[f.q1])
	}
	if s := fmt.Sprint(plan["starts_on_approval"]); s != fmt.Sprintf("[%d]", f.solo) {
		t.Fatalf("starts_on_approval = %s, want [%d]", s, f.solo)
	}
	outside := plan["outside"].([]any)
	found := false
	for _, x := range outside {
		o := x.(map[string]any)
		if int(o["id"].(float64)) == f.build {
			found = o["relation"] == "dependency" && o["satisfied"] == false && o["merge_state"] == domain.MergeNeeded && o["epic_name"] != ""
		}
	}
	if !found {
		t.Fatalf("build (done, unmerged) missing from outside: %v", outside)
	}
	if plan["as_of"] == nil || plan["caller_last_approved_at"] == nil || plan["can_approve_on_behalf"] != false {
		t.Fatalf("plan header = as_of %v last %v on_behalf %v", plan["as_of"], plan["caller_last_approved_at"], plan["can_approve_on_behalf"])
	}
	hasIssue := false
	for _, x := range plan["issues"].([]any) {
		iss := x.(map[string]any)
		if iss["kind"] == "isolated" && slices.Contains(iss["task_ids"].([]any), any(float64(f.solo))) {
			hasIssue = true
		}
	}
	if !hasIssue {
		t.Fatalf("plan issues = %v", plan["issues"])
	}

	// Approving deploy and re-planning it shows it as changed.
	approveAs(t, f.ra, "alice", pid, f.deploy)
	f.w.s.as(f.w.alice).call(t, "amp_update_task", map[string]any{"task_id": float64(f.deploy), "description": "deploy, now to two regions"})
	one := f.ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work?approve=%d&plan_limit=2", pid, f.w.s.epicID), nil)["plan"].(map[string]any)
	if fmt.Sprint(one["changed_ids"]) != fmt.Sprintf("[%d]", f.deploy) {
		t.Fatalf("changed_ids = %v", one["changed_ids"])
	}
	if len(one["tickets"].([]any)) != 2 || one["more"] != true || int(one["limit"].(float64)) != 2 {
		t.Fatalf("plan paging = %d tickets, more %v", len(one["tickets"].([]any)), one["more"])
	}
	all := f.ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work?approve=%d", pid, f.w.s.epicID), nil)["plan"].(map[string]any)
	for _, x := range all["tickets"].([]any) {
		pt := x.(map[string]any)
		if int(pt["id"].(float64)) == f.deploy {
			cs, _ := pt["changed_since"].(map[string]any)
			if pt["change"] != "changed" || cs == nil || cs["at"] == nil || cs["last_approved_at"] == nil {
				t.Fatalf("changed deploy = %v", pt)
			}
		}
	}

	// Recording the merge and answering a question clear them, and both
	// show in done today (the merge with who recorded it).
	f.ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/merge", f.build), nil)
	f.ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/unblock", f.q2), map[string]any{"answer": "eu-west"})
	out = f.ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work", pid), nil)
	if n := len(out["items"].(map[string]any)["merge"].([]any)); n != 0 {
		t.Fatalf("merge items after the merge = %d", n)
	}
	if ids := itemTaskIDs(out["items"].(map[string]any)["answer"]); !slices.Equal(ids, []int{f.q1}) {
		t.Fatalf("answer after answering q2 = %v", ids)
	}
	var sawMerge, sawApprove, sawAnswer bool
	for _, x := range out["done_today"].([]any) {
		dd := x.(map[string]any)
		switch dd["kind"] {
		case "merge":
			sawMerge = int(dd["task_id"].(float64)) == f.build && dd["merge_source"] == domain.MergeSourceUI
		case "approve":
			sawApprove = sawApprove || int(dd["count"].(float64)) >= 1
		case "answer":
			sawAnswer = int(dd["task_id"].(float64)) == f.q2
		}
	}
	if !sawMerge || !sawApprove || !sawAnswer {
		t.Fatalf("done_today = %v", out["done_today"])
	}
	if a := out["agents"].(map[string]any); a["done_today"].(float64) < 1 {
		t.Fatalf("agents = %v", a)
	}
}

// The project tab, the summary and the badge count the same things.
func TestWaitingOn_CountsAgreeAndSummaryPages(t *testing.T) {
	f := newWaitFixture(t)
	pid := f.w.s.projectID
	tab := countsOf(f.ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work", pid), nil)["counts"])
	tab2 := countsOf(f.ra.want(t, 200, "alice", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work", f.proj2), nil)["counts"])
	badge := f.ra.want(t, 200, "alice", http.MethodGet, "/api/my-work/counts", nil)
	sum := f.ra.want(t, 200, "alice", http.MethodGet, "/api/my-work?rows=2&projects_limit=1", nil)

	bp := badge["projects"].(map[string]any)
	if b := countsOf(bp[fmt.Sprint(pid)]); fmt.Sprint(b) != fmt.Sprint(tab) {
		t.Fatalf("badge %v != tab %v", b, tab)
	}
	if b := countsOf(bp[fmt.Sprint(f.proj2)]); fmt.Sprint(b) != fmt.Sprint(tab2) || b["answer"] != 1 {
		t.Fatalf("badge %v != tab %v (project 2)", b, tab2)
	}
	total := countsOf(badge["counts"])
	if total["waiting"] != tab["waiting"]+tab2["waiting"] || fmt.Sprint(countsOf(sum["counts"])) != fmt.Sprint(total) {
		t.Fatalf("totals: badge %v, summary %v, tabs %v + %v", total, sum["counts"], tab, tab2)
	}

	// Paging: the busiest project first, 2 rows of it, the rest counted.
	projects := sum["projects"].([]any)
	if len(projects) != 1 || sum["projects_more"] != true || int(sum["projects_total"].(float64)) != 2 {
		t.Fatalf("project paging = %d cards, more %v, total %v", len(projects), sum["projects_more"], sum["projects_total"])
	}
	card := projects[0].(map[string]any)
	if int(card["project_id"].(float64)) != pid || fmt.Sprint(countsOf(card["counts"])) != fmt.Sprint(tab) {
		t.Fatalf("first card = %v", card)
	}
	if rows := card["rows"].([]any); len(rows) != 2 || int(card["more"].(float64)) != tab["waiting"]-2 {
		t.Fatalf("rows %d, more %v, waiting %d", len(rows), card["more"], tab["waiting"])
	}
	if r := card["rows"].([]any)[0].(map[string]any); r["kind"] != "approve" || r["plan"] != nil {
		t.Fatalf("summary row = %v", r)
	}
	next := f.ra.want(t, 200, "alice", http.MethodGet, "/api/my-work?projects_offset=1&projects_limit=1", nil)
	if p := next["projects"].([]any); len(p) != 1 || int(p[0].(map[string]any)["project_id"].(float64)) != f.proj2 || next["projects_more"] != false {
		t.Fatalf("second page = %v", next["projects"])
	}
	// The older fields are still there.
	for _, k := range []string{"owned", "flags", "agents", "done_24h", "owned_truncated", "flags_truncated"} {
		if _, ok := sum[k]; !ok {
			t.Fatalf("GET /api/my-work lost %q", k)
		}
	}
	if sum["owned_truncated"] != false {
		t.Fatalf("owned_truncated = %v", sum["owned_truncated"])
	}
	if bad, _ := f.ra.do(t, "alice", http.MethodGet, "/api/my-work?rows=x", nil); bad != 400 {
		t.Fatalf("rows=x = %d, want 400", bad)
	}

	// Bob: only his human ticket (do), nothing of alice's.
	bob := countsOf(f.ra.want(t, 200, "bob", http.MethodGet, "/api/my-work/counts", nil)["counts"])
	if bob["do"] != 1 || bob["waiting"] != 1 {
		t.Fatalf("bob's counts = %v", bob)
	}
}

// Someone else's list: a lead may read it (read-only), a member may not,
// and a non-member doesn't see the project.
func TestWaitingOn_Permissions(t *testing.T) {
	f := newWaitFixture(t)
	pid := f.w.s.projectID
	path := fmt.Sprintf("/api/projects/%d/my-work?user=%s", pid, f.w.alice.Email)
	f.ra.want(t, 403, "bob", http.MethodGet, path, nil)
	f.ra.want(t, 403, "bob", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work?user=%d&approve=%d", pid, f.w.alice.User.ID, f.w.s.epicID), nil)
	f.ra.want(t, 404, "outsider", http.MethodGet, path, nil)
	lead := f.ra.want(t, 200, "lead", http.MethodGet, path, nil)
	if lead["read_only"] != true || int(lead["user_id"].(float64)) != f.w.alice.User.ID || countsOf(lead["counts"])["answer"] != 2 {
		t.Fatalf("lead reading alice's list = %v", lead)
	}
	// The plan says the lead may approve on alice's behalf, and none is the lead's own.
	plan := lead["items"].(map[string]any)["approve"].([]any)[0].(map[string]any)["plan"].(map[string]any)
	if plan["can_approve_on_behalf"] != true || len(plan["yours_ids"].([]any)) != 0 || len(plan["for_user_ids"].([]any)) != 3 {
		t.Fatalf("lead's view of the plan = on_behalf %v yours %v for_user %v", plan["can_approve_on_behalf"], plan["yours_ids"], plan["for_user_ids"])
	}
	// Bob reads his own (any member).
	own := f.ra.want(t, 200, "bob", http.MethodGet, fmt.Sprintf("/api/projects/%d/my-work?user=me", pid), nil)
	if ids := itemTaskIDs(own["items"].(map[string]any)["do"]); !slices.Equal(ids, []int{f.bobsHuman}) {
		t.Fatalf("bob's do = %v", ids)
	}
	// The outsider's summary and badge hold nothing of this project.
	if c := countsOf(f.ra.want(t, 200, "outsider", http.MethodGet, "/api/my-work/counts", nil)["counts"]); c["waiting"] != 0 {
		t.Fatalf("outsider counts = %v", c)
	}
}
