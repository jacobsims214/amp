package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"
)

// Metadata: free-form key/value data on projects, epics, stories, and
// tasks, set and changed over MCP, inherited downwards, and searchable.
func TestMetadata_SetInheritFind(t *testing.T) {
	w := newRBACWorld(t)
	pid := float64(w.s.projectID)
	alice := w.as(w.alice)

	// Project owners set project metadata; members can't.
	wantErr(t, alice.callExpectError(t, "amp_update_project_settings", map[string]interface{}{"project_id": pid, "metadata": map[string]interface{}{"x": "y"}}), "requires owner access")
	w.as(w.owner).call(t, "amp_update_project_settings", map[string]interface{}{"project_id": pid,
		"metadata": map[string]interface{}{"jira_board": "PLAT", "capitalizable": true, "initiative": "Q3 growth"}})

	epic := alice.call(t, "amp_create_epic", map[string]interface{}{"project_id": pid, "name": "Checkout",
		"metadata": map[string]interface{}{"initiative": "Payments", "budget_code": 4200}})
	epicID := intField(t, epic, "id")
	story := alice.call(t, "amp_create_story", map[string]interface{}{"project_id": pid, "epic_id": float64(epicID), "name": "Card payments",
		"metadata": map[string]interface{}{"capitalizable": false}})
	storyID := intField(t, story, "id")
	task := alice.call(t, "amp_create_task", map[string]interface{}{"project_id": pid, "epic_id": float64(epicID), "story_id": float64(storyID),
		"name": "Stripe webhook", "description": "d", "acceptance_criteria": "a", "assigned_to": "amp-worker",
		"metadata": map[string]interface{}{"jira_key": "PLAT-123"}})
	tid := taskID(t, task)

	// Effective metadata: project, then epic, then story, then task (later wins).
	got := alice.call(t, "amp_get_task", map[string]interface{}{"task_id": float64(tid)})
	eff := got["effective_metadata"].(map[string]interface{})
	want := map[string]interface{}{"jira_board": "PLAT", "initiative": "Payments", "budget_code": float64(4200), "capitalizable": false, "jira_key": "PLAT-123"}
	if fmt.Sprint(eff) != fmt.Sprint(want) {
		t.Fatalf("task effective metadata = %v, want %v", eff, want)
	}
	if own := got["task"].(map[string]interface{})["metadata"].(map[string]interface{}); len(own) != 1 {
		t.Fatalf("task's own metadata = %v", own)
	}

	// Changes merge; null removes. The task actor's copy stays current.
	alice.call(t, "amp_update_task", map[string]interface{}{"task_id": float64(tid), "metadata": map[string]interface{}{"jira_key": nil, "odoo_task": 77}})
	got = alice.call(t, "amp_get_task", map[string]interface{}{"task_id": float64(tid)})
	own := got["task"].(map[string]interface{})["metadata"].(map[string]interface{})
	if _, ok := own["jira_key"]; ok || own["odoo_task"] != float64(77) {
		t.Fatalf("task metadata after change = %v", own)
	}
	alice.call(t, "amp_update_epic", map[string]interface{}{"epic_id": float64(epicID), "metadata": map[string]interface{}{"budget_code": nil}})
	e := alice.call(t, "amp_get_epic", map[string]interface{}{"epic_id": float64(epicID)})
	if m := e["epic"].(map[string]interface{})["metadata"].(map[string]interface{}); fmt.Sprint(m) != "map[initiative:Payments]" {
		t.Fatalf("epic metadata = %v", m)
	}
	if eff := e["effective_metadata"].(map[string]interface{}); eff["jira_board"] != "PLAT" {
		t.Fatalf("epic effective metadata = %v", eff)
	}

	// Find by effective metadata, typed: the story overrides capitalizable.
	res := alice.call(t, "amp_find_by_metadata", map[string]interface{}{"kind": "task", "match": map[string]interface{}{"jira_board": "PLAT", "initiative": "Payments"}})
	if res["count"].(float64) != 1 {
		t.Fatalf("find task by inherited metadata = %v", res)
	}
	if found := foundIDs(alice.call(t, "amp_find_by_metadata", map[string]interface{}{"kind": "story", "match": map[string]interface{}{"capitalizable": true}})); slices.Contains(found, storyID) || !slices.Contains(found, w.s.storyID) {
		t.Fatalf("capitalizable stories = %v: the card payments story (%d) overrides it to false, the shared story (%d) inherits true", found, storyID, w.s.storyID)
	}
	if found := foundIDs(alice.call(t, "amp_find_by_metadata", map[string]interface{}{"kind": "epic", "match": map[string]interface{}{"capitalizable": true}})); !slices.Contains(found, epicID) {
		t.Fatalf("the epic inherits capitalizable=true, found %v", found)
	}
	// "true" the string is not true the boolean.
	res = alice.call(t, "amp_find_by_metadata", map[string]interface{}{"kind": "epic", "match": map[string]interface{}{"capitalizable": "true"}})
	if res["count"].(float64) != 0 {
		t.Fatalf("string matched a boolean: %v", res)
	}
	// Only projects you can see.
	res = w.as(w.outsider).call(t, "amp_find_by_metadata", map[string]interface{}{"kind": "project", "match": map[string]interface{}{"jira_board": "PLAT"}})
	if res["count"].(float64) != 0 {
		t.Fatalf("outsider found %v", res)
	}
	wantErr(t, w.as(w.outsider).callExpectError(t, "amp_find_by_metadata", map[string]interface{}{"kind": "project", "project_id": pid, "match": map[string]interface{}{"x": "y"}}), "not found")

	// Validation.
	for _, bad := range []map[string]interface{}{
		{"nested": map[string]interface{}{"a": 1}},
		{"list": []interface{}{1, 2}},
		{" padded": "x"},
	} {
		wantErr(t, alice.callExpectError(t, "amp_update_task", map[string]interface{}{"task_id": float64(tid), "metadata": bad}), "metadata")
	}
	wantErr(t, alice.callExpectError(t, "amp_find_by_metadata", map[string]interface{}{"kind": "widget", "match": map[string]interface{}{"x": "y"}}), "kind must be")
	wantErr(t, alice.callExpectError(t, "amp_find_by_metadata", map[string]interface{}{"kind": "task", "match": map[string]interface{}{"jira_key": nil}}), "can't be null")

	// Metadata survives export and import.
	bundle := w.as(w.owner).call(t, "amp_export_project", map[string]interface{}{"project_id": pid})
	raw, _ := json.Marshal(bundle)
	imported := w.as(w.owner).call(t, "amp_import_project", map[string]interface{}{"bundle_json": string(raw), "code": fmt.Sprintf("meta-%d", time.Now().UnixNano())})
	newID := int(imported["project"].(map[string]interface{})["id"].(float64))
	p, _ := w.s.repo.GetProject(context.Background(), newID)
	if p.Metadata["jira_board"] != "PLAT" {
		t.Fatalf("imported project metadata = %v", p.Metadata)
	}
	stories, _ := w.s.repo.ListStoriesByProject(context.Background(), newID)
	found := false
	for _, st := range stories {
		if st.Name == "Card payments" && st.Metadata["capitalizable"] == false {
			found = true
		}
	}
	if !found {
		t.Fatalf("imported story lost its metadata: %+v", stories)
	}
}

func foundIDs(res map[string]interface{}) []int {
	var out []int
	for _, r := range res["results"].([]interface{}) {
		out = append(out, int(r.(map[string]interface{})["id"].(float64)))
	}
	return out
}
