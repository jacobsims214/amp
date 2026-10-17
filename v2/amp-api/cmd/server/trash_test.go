package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Deleted tickets go to the trash and come back whole: text, comments,
// history, and dependency links in both directions, with states following.
func TestTrash_DeleteAndRestore(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	root := w.s.as(w.root)
	a := mkIn(t, root, w.s.storyID, "schema")
	b := mkIn(t, root, w.s.storyID, "endpoints", a)
	c := mkIn(t, root, w.s.storyID, "ui", b)
	root.call(t, "amp_add_task_comment", map[string]any{"task_id": float64(b), "body": "decided: REST, not gRPC"})

	// Wipe b and c (as the wargame's manager did with a whole plan).
	root.call(t, "amp_delete_task", map[string]any{"task_id": float64(c)})
	root.call(t, "amp_delete_task", map[string]any{"task_id": float64(b)})
	trash := root.call(t, "amp_list_deleted_tasks", map[string]any{"project_id": float64(w.s.projectID)})
	if trash["count"] != 2.0 {
		t.Fatalf("trash: %v", trash)
	}

	res := ra.want(t, 200, "root", http.MethodPost, fmt.Sprintf("/api/projects/%d/trash/restore", w.s.projectID), map[string]any{"task_ids": []int{c, b}})
	for _, r := range res["results"].([]any) {
		if m := r.(map[string]any); m["restored"] != true || m["error"] != nil {
			t.Fatalf("restore: %v", res)
		}
	}
	hist := root.call(t, "amp_get_ticket_history", map[string]any{"task_id": float64(b)})
	if len(hist["comments"].([]any)) != 1 {
		t.Fatalf("b's comment didn't come back: %v", hist["comments"])
	}
	task := hist["task"].(map[string]any)
	if fmt.Sprint(task["dependency_ids"]) != fmt.Sprintf("[%d]", a) {
		t.Fatalf("b's dependency on a: %v", task["dependency_ids"])
	}
	// c depended on b; restored in either order, the link is back and c waits.
	ct := root.call(t, "amp_get_task", map[string]any{"task_id": float64(c)})
	if v, ok := ct["task"]; ok {
		ct = v.(map[string]any)
	}
	if fmt.Sprint(ct["dependency_ids"]) != fmt.Sprintf("[%d]", b) || ct["state"] != "blocked" {
		t.Fatalf("c after restore: %v", ct)
	}
	// Completing the chain still flows through the restored tickets.
	root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(a), "agent_id": "x"})
	root.call(t, "amp_complete_task", map[string]any{"task_id": float64(a)})
	if st := stateOf(t, w.s, b); st != "backlog" {
		t.Fatalf("b after a completed: %s", st)
	}
	// The trash is empty again, and a non-lead who didn't delete can't restore.
	if tr := root.call(t, "amp_list_deleted_tasks", map[string]any{"project_id": float64(w.s.projectID)}); tr["count"] != 0.0 {
		t.Fatalf("trash after restore: %v", tr)
	}
	d := mkIn(t, root, w.s.storyID, "spike")
	root.call(t, "amp_delete_task", map[string]any{"task_id": float64(d)})
	out := w.s.as(w.alice).call(t, "amp_restore_tasks", map[string]any{"project_id": float64(w.s.projectID), "task_ids": []any{float64(d)}})
	if r := out["results"].([]any)[0].(map[string]any); r["restored"] == true {
		t.Fatalf("a member restored someone else's deletion: %v", out)
	}
}

// Deleted stories and epics come back with their original IDs, their
// tickets, and the links into the rest of the plan, and the restored work
// runs like any other.
func TestTrash_RestoreStoryAndEpic(t *testing.T) {
	w := newRBACWorld(t)
	root := w.s.as(w.root)
	pid := float64(w.s.projectID)
	epic := intField(t, root.call(t, "amp_create_epic", map[string]any{"project_id": pid, "name": "quotes", "metadata": map[string]any{"capitalizable": "yes"}}), "id")
	story := intField(t, root.call(t, "amp_create_story", map[string]any{"project_id": pid, "epic_id": float64(epic), "name": "quote api", "acceptance_criteria": "a"}), "id")
	a := mkIn(t, root, story, "contract")
	b := mkIn(t, root, story, "endpoint", a)
	x := mkIn(t, root, w.s.storyID, "ui outside the epic", b) // depends into the story

	listGroups := func() []any {
		tr := root.call(t, "amp_list_deleted_tasks", map[string]any{"project_id": pid})
		g, _ := tr["deleted_epics_and_stories"].([]any)
		return g
	}
	restore := func(args map[string]any) map[string]any {
		t.Helper()
		args["project_id"] = pid
		return root.call(t, "amp_restore_tasks", args)
	}
	groupOK := func(out map[string]any) {
		t.Helper()
		for _, g := range out["groups"].([]any) {
			m := g.(map[string]any)
			if m["restored"] != true || m["error"] != nil {
				t.Fatalf("group restore: %v", out)
			}
			for _, tk := range m["tickets"].([]any) {
				if tk.(map[string]any)["restored"] != true {
					t.Fatalf("ticket restore: %v", out)
				}
			}
		}
	}
	checkLinks := func(when string) {
		t.Helper()
		bt := root.call(t, "amp_get_ticket_history", map[string]any{"task_id": float64(b)})["task"].(map[string]any)
		if fmt.Sprint(bt["dependency_ids"]) != fmt.Sprintf("[%d]", a) || int(bt["story_id"].(float64)) != story {
			t.Fatalf("%s: b = %v", when, bt)
		}
		xt := root.call(t, "amp_get_ticket_history", map[string]any{"task_id": float64(x)})["task"].(map[string]any)
		if fmt.Sprint(xt["dependency_ids"]) != fmt.Sprintf("[%d]", b) {
			t.Fatalf("%s: x lost its link to b: %v", when, xt["dependency_ids"])
		}
	}

	// A story.
	root.call(t, "amp_delete_story", map[string]any{"story_id": float64(story)})
	if g := listGroups(); len(g) != 1 || g[0].(map[string]any)["kind"] != "story" {
		t.Fatalf("trash after deleting the story: %v", g)
	}
	groupOK(restore(map[string]any{"story_ids": []any{float64(story)}}))
	checkLinks("after restoring the story")

	// An epic: its story and tickets come back too, with its metadata.
	root.call(t, "amp_delete_epic", map[string]any{"epic_id": float64(epic)})
	groupOK(restore(map[string]any{"epic_ids": []any{float64(epic)}}))
	checkLinks("after restoring the epic")
	e := root.call(t, "amp_get_epic", map[string]any{"epic_id": float64(epic)})
	if v, ok := e["epic"]; ok {
		e = v.(map[string]any)
	}
	if meta, _ := e["metadata"].(map[string]any); meta["capitalizable"] != "yes" {
		t.Fatalf("epic metadata: %v", e)
	}
	if len(listGroups()) != 0 {
		t.Fatalf("trash not empty: %v", listGroups())
	}

	// The restored actors run work: a → b → x flows.
	for _, id := range []int{a, b} {
		root.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(id), "agent_id": "w"})
		root.call(t, "amp_complete_task", map[string]any{"task_id": float64(id)})
	}
	if st := stateOf(t, w.s, x); st != "backlog" {
		t.Fatalf("x after the restored chain completed: %s", st)
	}

	// A story deleted before its epic: restoring the story alone is refused
	// until the epic is back; both in one call works.
	s2 := intField(t, root.call(t, "amp_create_story", map[string]any{"project_id": pid, "epic_id": float64(epic), "name": "pdf", "acceptance_criteria": "a"}), "id")
	c := mkIn(t, root, s2, "render pdf")
	root.call(t, "amp_delete_story", map[string]any{"story_id": float64(s2)})
	root.call(t, "amp_delete_epic", map[string]any{"epic_id": float64(epic)})
	out := restore(map[string]any{"story_ids": []any{float64(s2)}})
	if g := out["groups"].([]any)[0].(map[string]any); g["restored"] == true || !strings.Contains(fmt.Sprint(g["error"]), "restore the epic first") {
		t.Fatalf("story without its epic: %v", out)
	}
	groupOK(restore(map[string]any{"epic_ids": []any{float64(epic)}, "story_ids": []any{float64(s2)}}))
	if st := stateOf(t, w.s, c); st != "backlog" {
		t.Fatalf("c after restoring its epic and story: %s", st)
	}

	// Only a lead or whoever deleted it restores it.
	root.call(t, "amp_delete_story", map[string]any{"story_id": float64(s2)})
	out = w.s.as(w.alice).call(t, "amp_restore_tasks", map[string]any{"project_id": pid, "story_ids": []any{float64(s2)}})
	if g := out["groups"].([]any)[0].(map[string]any); g["restored"] == true {
		t.Fatalf("a member restored someone else's deletion: %v", out)
	}
}
