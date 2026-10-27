package main

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

// MCP's view of "Waiting on you" (AMP #2482, KB decisions/human-work-ux.md
// §1 and §5): amp_list_tasks' waiting_on_people is built from WaitingOn
// (all four kinds, each with kind) plus coming_up; amp_get_plan's counts gain
// waiting and questions; amp_list_unmerged gains merge_state and keeps
// abandoned tickets. Every field older managers read stays.

func waitKinds(v any) (ids map[string][]int, byID map[int]map[string]any) {
	ids, byID = map[string][]int{}, map[int]map[string]any{}
	for _, x := range v.([]any) {
		m := x.(map[string]any)
		id := int(m["id"].(float64))
		ids[m["kind"].(string)] = append(ids[m["kind"].(string)], id)
		byID[id] = m
	}
	return ids, byID
}

func TestWaitingOn_MCPListTasksKinds(t *testing.T) {
	f := newWaitFixture(t)
	ctx := context.Background()
	w := f.w
	alice := w.s.as(w.alice)
	pid := w.s.projectID

	// One more merge item: a PR closed without merging (abandoned).
	gone := mkIn(t, alice, w.s.storyID, "abandoned work")
	approveAs(t, f.ra, "alice", pid, gone)
	finishWith(t, alice, gone, map[string]any{"pr_url": "https://git.example/pr/" + uniq("gone"), "pushed": true})
	svc := service.New(w.s.repo, w.s.reg, w.s.kb)
	if _, err := svc.MarkMergeAbandoned(ctx, tokenPrincipal(w.alice), gone, domain.MergeSourceGitHub); err != nil {
		t.Fatal(err)
	}

	for _, format := range []string{"compact", "full"} {
		board := alice.call(t, "amp_list_tasks", map[string]any{"project_id": float64(pid), "owner": "any", "format": format})
		kinds, byID := waitKinds(board["waiting_on_people"])

		// Kinds in display order: approve, answer, merge, do.
		var order []string
		for _, x := range board["waiting_on_people"].([]any) {
			k := x.(map[string]any)["kind"].(string)
			if len(order) == 0 || order[len(order)-1] != k {
				order = append(order, k)
			}
		}
		if !slices.Equal(order, []string{"approve", "answer", "merge", "do"}) {
			t.Fatalf("%s: kind order = %v", format, order)
		}

		// approve: one per epic, id is a ticket in it, task_ids all of them.
		if len(kinds["approve"]) != 2 {
			t.Fatalf("%s: approve = %v", format, kinds["approve"])
		}
		var epics []int
		for _, id := range kinds["approve"] {
			a := byID[id]
			epics = append(epics, int(a["epic_id"].(float64)))
			if a["owner"] != w.alice.Email || a["state"] != "awaiting_approval" || a["reason"] != "approve" ||
				!slices.Contains(a["task_ids"].([]any), any(float64(id))) || a["name"] == "" || a["epic_name"] == nil {
				t.Fatalf("%s: approve item = %v", format, a)
			}
		}
		if !slices.Contains(epics, w.s.epicID) || !slices.Contains(epics, f.epic2) {
			t.Fatalf("%s: approve epics = %v", format, epics)
		}

		// answer: q1 before q2, with the question.
		if !slices.Equal(kinds["answer"], []int{f.q1, f.q2}) || byID[f.q1]["question"] != "Which provider?" || byID[f.q1]["state"] != "question" {
			t.Fatalf("%s: answer = %v %v", format, kinds["answer"], byID[f.q1])
		}

		// merge: build (needed, holding up deploy) and the abandoned one.
		if !slices.Equal(kinds["merge"], []int{f.build, gone}) {
			t.Fatalf("%s: merge = %v, want [%d %d]", format, kinds["merge"], f.build, gone)
		}
		b := byID[f.build]
		if b["merge_state"] != domain.MergeNeeded || b["pr_url"] == nil || b["state"] != "completed" ||
			!slices.Contains(b["holding_up"].([]any), any(float64(f.deploy))) {
			t.Fatalf("%s: merge item = %v", format, b)
		}
		if byID[gone]["merge_state"] != domain.MergeAbandoned {
			t.Fatalf("%s: abandoned = %v", format, byID[gone])
		}

		// do: alice's two and bob's, with the original fields unchanged.
		do := kinds["do"]
		slices.Sort(do)
		want := []int{f.doReady, f.doStarted, f.bobsHuman}
		slices.Sort(want)
		if !slices.Equal(do, want) {
			t.Fatalf("%s: do = %v, want %v", format, do, want)
		}
		r, s, bh := byID[f.doReady], byID[f.doStarted], byID[f.bobsHuman]
		if r["state"] != "ready" || r["reason"] != "review" || r["owner"] != w.alice.Email || r["name"] != "sign off" ||
			s["state"] != "in_progress" || bh["owner"] != w.bob.Email || bh["state"] != "ready" {
			t.Fatalf("%s: do items = %v / %v / %v", format, r, s, bh)
		}
		for _, k := range []string{"id", "name", "reason", "owner", "state", "waiting_hours", "kind", "task_ids", "since"} {
			if _, ok := r[k]; !ok {
				t.Fatalf("%s: do item lacks %q: %v", format, k, r)
			}
		}

		// coming_up: shown apart, never in waiting_on_people.
		cu, cuByID := waitKinds(board["coming_up"])
		if !slices.Equal(cu["coming_up"], []int{f.comingDeps, f.comingApproval}) {
			t.Fatalf("%s: coming_up = %v", format, board["coming_up"])
		}
		if c := cuByID[f.comingDeps]; c["state"] != "waiting" || fmt.Sprint(c["waits_on"]) != fmt.Sprintf("[%d]", f.deploy) {
			t.Fatalf("%s: coming_up deps = %v", format, c)
		}
		if c := cuByID[f.comingApproval]; c["state"] != "awaiting_approval" || c["awaiting_approval"] != true {
			t.Fatalf("%s: coming_up approval = %v", format, c)
		}
		for _, id := range []int{f.comingDeps, f.comingApproval} {
			if byID[id] != nil {
				t.Fatalf("%s: coming up ticket %d listed as waiting", format, id)
			}
		}
	}

	// Bob's default view (his own work): only his ticket, nothing of alice's.
	bob := w.s.as(w.bob).call(t, "amp_list_tasks", map[string]any{"project_id": float64(pid)})
	if kinds, _ := waitKinds(bob["waiting_on_people"]); len(kinds) != 1 || !slices.Equal(kinds["do"], []int{f.bobsHuman}) {
		t.Fatalf("bob's waiting_on_people = %v", bob["waiting_on_people"])
	}

	// amp_get_plan: blocked keeps its count; waiting and questions split it.
	plan := alice.call(t, "amp_get_plan", map[string]any{"project_id": float64(pid), "epic_id": float64(w.s.epicID)})
	tk := plan["epics"].([]any)[0].(map[string]any)["tickets"].(map[string]any)
	n := func(k string) int {
		v, _ := tk[k].(float64)
		return int(v)
	}
	// blocked: q1, q2 (questions); deploy (on build's merge) and comingDeps (waiting).
	if n("questions") != 2 || n("waiting") != 2 || n("blocked") != 4 || n("completed") != 2 {
		t.Fatalf("plan counts = %v", tk)
	}

	// amp_list_unmerged: merge_state on each, abandoned included.
	um := alice.call(t, "amp_list_unmerged", map[string]any{"project_id": float64(pid)})
	states := map[int]string{}
	for _, x := range um["waiting_to_merge"].([]any) {
		m := x.(map[string]any)
		if m["pr_url"] == nil || m["task_id"] == nil {
			t.Fatalf("unmerged run lost its fields: %v", m)
		}
		states[int(m["task_id"].(float64))] = m["merge_state"].(string)
	}
	if states[f.build] != domain.MergeNeeded || states[gone] != domain.MergeAbandoned || int(um["count"].(float64)) != len(states) {
		t.Fatalf("unmerged = %v", um)
	}
}
