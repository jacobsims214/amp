package main

import (
	"context"
	"fmt"
	"net/http"
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

// Merge-needed signal (AMP #2475, KB decisions/human-work-ux.md §4).

// mergeChain makes build -> deploy as alice and returns their ids.
func mergeChain(t *testing.T, w *rbacWorld, name string) (build, deploy int) {
	t.Helper()
	alice := w.s.as(w.alice)
	build = mkIn(t, alice, w.s.storyID, name+" build")
	deploy = mkIn(t, alice, w.s.storyID, name+" deploy", build)
	return build, deploy
}

// finishWith dispatches id and completes it over MCP with the run fields.
func finishWith(t *testing.T, s *mcpStack, id int, run map[string]any) {
	t.Helper()
	s.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(id), "agent_id": "amp-worker"})
	args := map[string]any{"task_id": float64(id)}
	for k, v := range run {
		args[k] = v
	}
	s.call(t, "amp_complete_task", args)
}

func mergeStateOf(t *testing.T, s *mcpStack, id int) string {
	t.Helper()
	ms, _ := planTask(t, s, id)["merge_state"].(string)
	return ms
}

func unmergedIDs(t *testing.T, s *mcpStack) string {
	t.Helper()
	um := s.call(t, "amp_list_unmerged", map[string]any{"project_id": float64(s.projectID)})
	return fmt.Sprint(um)
}

func uniq(prefix string) string { return fmt.Sprintf("%s-%x", prefix, time.Now().UnixNano()) }

func uiPrincipal(id *auth.Identity) authz.Principal {
	return authz.Principal{UserID: id.User.ID, Email: id.User.Email, Channel: auth.ChannelUI}
}

func tokenPrincipal(id *auth.Identity) authz.Principal {
	return authz.Principal{UserID: id.User.ID, Email: id.User.Email, Channel: auth.ChannelToken}
}

// Completion with a PR leaves the ticket done with merge_state needed; its
// dependent waits, saying why, and the ticket is in the unmerged queue. A
// merge recorded in the web UI releases it, once: a second report is a
// no-op returning the first record.
func TestMerge_NeededHoldsDependentsUntilMarkedMergedInUI(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	build, deploy := mergeChain(t, w, "ui")
	pr := "https://git.example/pr/" + uniq("ui")

	finishWith(t, alice, build, map[string]any{"pr_url": pr, "pushed": true})
	b := planTask(t, alice, build)
	if b["state"] != "completed" || b["merge_state"] != domain.MergeNeeded || b["merge_required"] != true {
		t.Fatalf("build after completion with a PR = %v", b)
	}
	d := planTask(t, alice, deploy)
	if d["state"] != "blocked" {
		t.Fatalf("deploy while build's merge is needed = %v, want blocked", d["state"])
	}
	if wd, _ := d["waiting_detail"].(string); !strings.Contains(wd, fmt.Sprintf("Waiting on merge of #%d", build)) || !strings.Contains(wd, pr) {
		t.Fatalf("deploy waiting_detail = %q", wd)
	}
	if !strings.Contains(unmergedIDs(t, alice), pr) {
		t.Fatalf("build missing from the unmerged queue")
	}
	// An agent can't start or finish the dependent meanwhile.
	alice.callExpectError(t, "amp_dispatch_task", map[string]any{"task_id": float64(deploy), "agent_id": "amp-worker"})

	// Mark merged in the UI (bob, another project member, may record it).
	rec := ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/merge", build), nil)
	if rec["merge_state"] != domain.MergeMerged || rec["merge_source"] != domain.MergeSourceUI || rec["already_recorded"] != false {
		t.Fatalf("mark merged = %v", rec)
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after the merge = %s, want backlog", st)
	}
	if wd, _ := planTask(t, alice, deploy)["waiting_detail"].(string); wd != "" {
		t.Fatalf("deploy still says %q", wd)
	}
	if strings.Contains(unmergedIDs(t, alice), pr) {
		t.Fatalf("merged build still in the unmerged queue")
	}
	// Idempotent: the second report changes nothing and says who recorded it.
	again := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/merge", build), nil)
	if again["already_recorded"] != true || again["merge_source"] != domain.MergeSourceUI || !strings.Contains(fmt.Sprint(again["message"]), "already merged") {
		t.Fatalf("second mark merged = %v", again)
	}
	// The old path, POST /runs {merged:true}, is a no-op now too: it returns
	// the first merged run instead of adding one.
	runsBefore := len(alice.call(t, "amp_get_task", map[string]any{"task_id": float64(build), "include": []any{"runs"}})["runs"].([]any))
	r1 := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/runs", build), map[string]any{"merged": true})
	if r1["merged"] != true {
		t.Fatalf("runs merged = %v", r1)
	}
	if n := len(alice.call(t, "amp_get_task", map[string]any{"task_id": float64(build), "include": []any{"runs"}})["runs"].([]any)); n != runsBefore {
		t.Fatalf("a repeated merge report added a run: %d -> %d", runsBefore, n)
	}
	h := fmt.Sprint(alice.call(t, "amp_get_task", map[string]any{"task_id": float64(build), "include": []any{"history"}})["history"])
	if !strings.Contains(h, "merge_needed") || !strings.Contains(h, "Merge recorded by ui") {
		t.Fatalf("history = %s", h)
	}
}

// CI (amp_report_merged) and the GitHub webhook (MarkMergedFrom "github")
// release dependents the same way, and the first report wins.
func TestMerge_CIAndGitHubReportsReleaseOnce(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	url := tokenMCPServer(t, w)
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	ctx := context.Background()

	// CI by branch.
	ciBuild, ciDeploy := mergeChain(t, w, "ci")
	branch := uniq("feat/ci")
	finishWith(t, alice, ciBuild, map[string]any{"branch": branch, "pushed": true})
	if ms := mergeStateOf(t, alice, ciBuild); ms != domain.MergeNeeded {
		t.Fatalf("pushed branch: merge_state %q, want needed", ms)
	}
	if st := stateOf(t, alice, ciDeploy); st != "blocked" {
		t.Fatalf("ci deploy = %s, want blocked", st)
	}
	carolTok := mintToken(t, ra, "carol", "me")
	out, isErr := tokenToolCall(t, url, carolTok, "amp_report_merged", map[string]any{"branch": branch, "merge_commit": "abcdef1234567"})
	if isErr || out["count"] != 1.0 || fmt.Sprint(out["already_merged_task_ids"]) != "[]" {
		t.Fatalf("CI report = %v", out)
	}
	b := planTask(t, alice, ciBuild)
	if b["merge_state"] != domain.MergeMerged || b["merge_source"] != domain.MergeSourceCI || b["merged_at"] == nil {
		t.Fatalf("after CI: %v", b)
	}
	if st := stateOf(t, alice, ciDeploy); st != "backlog" {
		t.Fatalf("ci deploy after CI merge = %s, want backlog", st)
	}
	again, _ := tokenToolCall(t, url, carolTok, "amp_report_merged", map[string]any{"branch": branch})
	if again["count"] != 1.0 || fmt.Sprint(again["already_merged_task_ids"]) != fmt.Sprintf("[%d]", ciBuild) {
		t.Fatalf("second CI report = %v", again)
	}
	// A later webhook doesn't overwrite CI's record.
	recs, err := svc.MarkMergedFrom(ctx, tokenPrincipal(w.alice), "", branch, "", "", domain.MergeSourceGitHub)
	if err != nil || len(recs) != 1 || !recs[0].AlreadyRecorded || recs[0].MergeSource != domain.MergeSourceCI {
		t.Fatalf("late webhook = %+v %v", recs, err)
	}

	// GitHub's webhook by PR first.
	ghBuild, ghDeploy := mergeChain(t, w, "gh")
	pr := "https://github.example/acme/pull/" + uniq("gh")
	finishWith(t, alice, ghBuild, map[string]any{"pr_url": pr})
	if st := stateOf(t, alice, ghDeploy); st != "blocked" {
		t.Fatalf("gh deploy = %s, want blocked", st)
	}
	recs, err = svc.MarkMergedFrom(ctx, tokenPrincipal(w.alice), "", "", pr, "", domain.MergeSourceGitHub)
	if err != nil || len(recs) != 1 || recs[0].AlreadyRecorded || recs[0].MergeSource != domain.MergeSourceGitHub {
		t.Fatalf("webhook = %+v %v", recs, err)
	}
	if st := stateOf(t, alice, ghDeploy); st != "backlog" {
		t.Fatalf("gh deploy after webhook = %s, want backlog", st)
	}
	// Mark merged in the UI afterwards: no-op, names GitHub.
	rec := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/merge", ghBuild), nil)
	if rec["already_recorded"] != true || rec["merge_source"] != domain.MergeSourceGitHub || !strings.Contains(fmt.Sprint(rec["message"]), "recorded by github") {
		t.Fatalf("UI after webhook = %v", rec)
	}
}

// An abandoned PR keeps dependents waiting; "Not needed" releases them;
// reopening still works.
func TestMerge_AbandonedAndNotNeeded(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	ctx := context.Background()
	build, deploy := mergeChain(t, w, "abandon")
	pr := "https://git.example/pr/" + uniq("ab")
	finishWith(t, alice, build, map[string]any{"pr_url": pr})

	recs, err := svc.MarkAbandonedFrom(ctx, tokenPrincipal(w.alice), "", "", pr, domain.MergeSourceGitHub)
	if err != nil || len(recs) != 1 || recs[0].MergeState != domain.MergeAbandoned {
		t.Fatalf("abandon = %+v %v", recs, err)
	}
	d := planTask(t, alice, deploy)
	if d["state"] != "blocked" || !strings.Contains(fmt.Sprint(d["waiting_detail"]), "closed without merging") {
		t.Fatalf("deploy after abandon = %v", d)
	}
	// Someone who can't work the ticket can't wave it through.
	if code, _ := ra.do(t, "carol", http.MethodPost, fmt.Sprintf("/api/tasks/%d/merge/not-needed", build), nil); code < 400 {
		t.Fatalf("carol marked alice's merge not needed: %d", code)
	}
	rec := ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/merge/not-needed", build), nil)
	if rec["merge_state"] != domain.MergeNone {
		t.Fatalf("not needed = %v", rec)
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("deploy after not needed = %s, want backlog", st)
	}
	if strings.Contains(unmergedIDs(t, alice), pr) {
		t.Fatalf("not-needed ticket still in the unmerged queue")
	}
	// Nothing left to release.
	ra.want(t, 409, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/merge/not-needed", build), nil)
	// A late report of the same PR doesn't gate it again.
	alice.call(t, "amp_report_run", map[string]any{"task_id": float64(build), "outcome": "progress", "pr_url": pr})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNone {
		t.Fatalf("late report after not needed: %q", ms)
	}

	// Not needed from needed works too, and reopen keeps working.
	b2, d2 := mergeChain(t, w, "reopen")
	finishWith(t, alice, b2, map[string]any{"pr_url": pr + "-2"})
	if _, err := svc.MarkMergeAbandoned(ctx, tokenPrincipal(w.alice), b2, domain.MergeSourceGitHub); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetTaskState(ctx, uiPrincipal(w.alice), b2, domain.TaskStateBacklog, ""); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if st := stateOf(t, alice, b2); st != "backlog" {
		t.Fatalf("reopened = %s", st)
	}
	if st := stateOf(t, alice, d2); st != "blocked" {
		t.Fatalf("dependent of a reopened ticket = %s", st)
	}
	// It waits on the reopened ticket being done again, not on its closed PR.
	if wd := fmt.Sprint(planTask(t, alice, d2)["waiting_detail"]); strings.Contains(wd, "reopen it") || strings.Contains(wd, "closed without merging") {
		t.Fatalf("dependent of a reopened ticket still says %q", wd)
	}
	finishWith(t, alice, b2, map[string]any{"pr_url": pr + "-3"})
	if ms := mergeStateOf(t, alice, b2); ms != domain.MergeNeeded {
		t.Fatalf("re-completed with a new PR: %q, want needed", ms)
	}
	ra.want(t, 200, "alice", http.MethodPost, fmt.Sprintf("/api/tasks/%d/merge/not-needed", b2), nil)
	if st := stateOf(t, alice, d2); st != "backlog" {
		t.Fatalf("dependent after not needed = %s", st)
	}
}

// require_merge off is today's behaviour: dependents start on completion.
// Turning it off releases what waits; tickets completed before it was on
// (as before the migration) are never gated by a late PR report.
func TestMerge_RequireMergeOffAndPreexistingTickets(t *testing.T) {
	w := newRBACWorld(t)
	alice, owner := w.s.as(w.alice), w.s.as(w.owner)
	ctx := context.Background()

	if p, err := w.s.repo.GetProject(ctx, w.s.projectID); err != nil || !p.RequireMerge || p.RequireMergeSince == nil {
		t.Fatalf("new project require_merge = %+v %v, want on", p, err)
	}
	// One ticket waiting on its merge before the switch.
	held, heldDep := mergeChain(t, w, "held")
	finishWith(t, alice, held, map[string]any{"pr_url": "https://git.example/pr/" + uniq("held")})
	if st := stateOf(t, alice, heldDep); st != "blocked" {
		t.Fatalf("held dep = %s", st)
	}
	owner.call(t, "amp_update_project_settings", map[string]any{"project_id": float64(w.s.projectID), "require_merge": false})
	if st := stateOf(t, alice, heldDep); st != "backlog" {
		t.Fatalf("held dep after require_merge off = %s, want backlog", st)
	}
	build, deploy := mergeChain(t, w, "off")
	finishWith(t, alice, build, map[string]any{"pr_url": "https://git.example/pr/" + uniq("off"), "pushed": true})
	if ms := mergeStateOf(t, alice, build); ms != domain.MergeNone {
		t.Fatalf("require_merge off: merge_state %q, want none", ms)
	}
	if st := stateOf(t, alice, deploy); st != "backlog" {
		t.Fatalf("require_merge off: deploy = %s, want backlog", st)
	}
	// Back on: since it was off, require_merge_since moved.
	owner.call(t, "amp_update_project_settings", map[string]any{"project_id": float64(w.s.projectID), "require_merge": true})

	// A ticket completed before merges were required (as every ticket done
	// before the migration): a PR reported on it later gates nothing.
	old, oldDep := mergeChain(t, w, "old")
	finishWith(t, alice, old, nil)
	db, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	if _, err := db.Exec(ctx, `UPDATE tasks SET completed_at = NOW() - INTERVAL '30 days' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	alice.call(t, "amp_report_run", map[string]any{"task_id": float64(old), "outcome": "completed", "pr_url": "https://git.example/pr/" + uniq("old")})
	if ms := mergeStateOf(t, alice, old); ms != domain.MergeNone {
		t.Fatalf("pre-existing ticket gated by a late PR: %q", ms)
	}
	if st := stateOf(t, alice, oldDep); st != "backlog" {
		t.Fatalf("pre-existing ticket's dependent = %s, want backlog", st)
	}

	// The same late report on a ticket done after the switch does gate it,
	// and its waiting dependent goes back to blocked.
	late, lateDep := mergeChain(t, w, "late")
	finishWith(t, alice, late, nil)
	if st := stateOf(t, alice, lateDep); st != "backlog" {
		t.Fatalf("late dep = %s", st)
	}
	alice.call(t, "amp_report_run", map[string]any{"task_id": float64(late), "outcome": "completed", "pr_url": "https://git.example/pr/" + uniq("late")})
	if ms := mergeStateOf(t, alice, late); ms != domain.MergeNeeded {
		t.Fatalf("PR reported after completion: %q, want needed", ms)
	}
	if st := stateOf(t, alice, lateDep); st != "blocked" {
		t.Fatalf("late dep after the late PR = %s, want blocked", st)
	}
}

// A person's run with a PR link doesn't put their own ticket in the
// unmerged queue, and marking it done records that PR merged on the agent
// tickets that produced it (the bridge for legacy merge tickets).
func TestMerge_HumanTicketBridge(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	alice := w.s.as(w.alice)
	pr := "https://git.example/acme/pull/" + uniq("bridge")
	res := alice.call(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "build", "story_id": float64(w.s.storyID), "name": "Build it"},
		map[string]any{"ref": "next", "story_id": float64(w.s.storyID), "name": "Next step", "depends_on": []any{"build"}},
		map[string]any{"ref": "merge", "story_id": float64(w.s.storyID), "name": "Merge the PR", "performer": "human",
			"human_reason": "review", "owner": w.bob.Email},
	}})
	ids := map[string]int{}
	for _, c := range res["created"].([]any) {
		m := c.(map[string]any)
		ids[m["ref"].(string)] = int(m["id"].(float64))
	}
	finishWith(t, alice, ids["build"], map[string]any{"pr_url": pr})
	if st := stateOf(t, alice, ids["next"]); st != "blocked" {
		t.Fatalf("next = %s, want blocked on the merge", st)
	}
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/start", ids["merge"]), nil)
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", ids["merge"]), map[string]any{"note": "merged", "pr_url": pr})

	b := planTask(t, alice, ids["build"])
	if b["merge_state"] != domain.MergeMerged || b["merge_source"] != domain.MergeSourceUI {
		t.Fatalf("producing ticket after the bridge = %v", b)
	}
	if st := stateOf(t, alice, ids["next"]); st != "backlog" {
		t.Fatalf("next after the bridge = %s, want backlog", st)
	}
	if ms := mergeStateOf(t, alice, ids["merge"]); ms != domain.MergeNone {
		t.Fatalf("human ticket merge_state = %q, want none", ms)
	}
	q := w.s.as(w.root).call(t, "amp_list_unmerged", map[string]any{"project_id": float64(w.s.projectID)})
	if strings.Contains(fmt.Sprint(q), pr) {
		t.Fatalf("unmerged queue lists the PR: %v", q)
	}

	// A human ticket alone with a PR link (nothing produced it) isn't queued.
	solo := alice.call(t, "amp_create_tasks", map[string]any{"project_id": float64(w.s.projectID), "tasks": []any{
		map[string]any{"ref": "solo", "story_id": float64(w.s.storyID), "name": "Ship it", "performer": "human", "human_reason": "external", "owner": w.bob.Email},
	}})
	soloID := int(solo["created"].([]any)[0].(map[string]any)["id"].(float64))
	soloPR := "https://git.example/acme/pull/" + uniq("solo")
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/start", soloID), nil)
	ra.want(t, 200, "bob", http.MethodPost, fmt.Sprintf("/api/tasks/%d/complete", soloID), map[string]any{"pr_url": soloPR})
	if q := w.s.as(w.root).call(t, "amp_list_unmerged", map[string]any{"project_id": float64(w.s.projectID)}); strings.Contains(fmt.Sprint(q), soloPR) {
		t.Fatalf("a person's own ticket is in the unmerged queue: %v", q)
	}
}

// Migration 038 is additive: on a database at 037 with done tickets (one
// with a merged run, one with an unmerged PR), every ticket is merge_state
// none, projects require merges from now, nothing is gated, and it re-runs.
func TestMigration_038MergeStateIsAdditive(t *testing.T) {
	ctx := context.Background()
	dsn := freshDatabase(t)
	repo, err := repository.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	var before []repository.Migration
	for _, m := range migrations {
		if m.Version < "038" {
			before = append(before, m)
		}
	}
	if len(before) == len(migrations) {
		t.Fatal("no migration 038")
	}
	if err := repo.RunMigrations(ctx, before); err != nil {
		t.Fatalf("migrate to 037: %v", err)
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	q := func(sql string, args ...any) int {
		t.Helper()
		var id int
		if err := db.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	proj := q(`INSERT INTO projects (name, code) VALUES ('p', 'p') RETURNING id`)
	epic := q(`INSERT INTO epics (project_id, name) VALUES ($1, 'e') RETURNING id`, proj)
	story := q(`INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 's') RETURNING id`, proj, epic)
	done := q(`INSERT INTO tasks (project_id, epic_id, story_id, name, state, completed_at) VALUES ($1, $2, $3, 'done', 'completed', NOW() - INTERVAL '1 day') RETURNING id`, proj, epic, story)
	q(`INSERT INTO task_runs (task_id, project_id, outcome, pr_url, pushed) VALUES ($1, $2, 'completed', 'https://x/pr/1', TRUE) RETURNING id::int`, done, proj)
	dep := q(`INSERT INTO tasks (project_id, epic_id, story_id, name, state) VALUES ($1, $2, $3, 'dep', 'backlog') RETURNING id`, proj, epic, story)
	if _, err := db.Exec(ctx, `INSERT INTO task_dependencies (task_id, depends_on) VALUES ($1, $2)`, dep, done); err != nil {
		t.Fatal(err)
	}

	if err := repo.RunMigrations(ctx, migrations); err != nil {
		t.Fatalf("migrate 038: %v", err)
	}
	tk, err := repo.GetTask(ctx, done)
	if err != nil || tk.MergeState != domain.MergeNone || tk.MergeRequired || tk.MergedAt != nil {
		t.Fatalf("pre-existing done ticket after 038 = %+v %v", tk, err)
	}
	p, err := repo.GetProject(ctx, proj)
	if err != nil || !p.RequireMerge || p.RequireMergeSince == nil || p.RequireMergeSince.Before(*tk.CompletedAt) {
		t.Fatalf("project after 038 = %+v %v", p, err)
	}
	sat, err := repo.CompletedAmong(ctx, []int{done})
	if err != nil || !sat[done] {
		t.Fatalf("pre-existing done ticket gates its dependent: %v %v", sat, err)
	}
	// Idempotent: the SQL re-runs cleanly.
	for _, m := range migrations {
		if m.Version == "038" || strings.HasPrefix(m.Version, "038") {
			if _, err := db.Exec(ctx, m.SQL); err != nil {
				t.Fatalf("re-run 038: %v", err)
			}
		}
	}
}
