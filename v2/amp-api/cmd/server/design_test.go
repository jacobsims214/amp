package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The design system process, end to end: an org owner starts the org's
// design system (a Design System project with a fixed template of phase
// tickets), every phase needs an approved artifact before publishing, and
// design-system/ docs can only be written by publishing. A product project
// then builds its layer on top, and amp_design_context sees both.
func TestDesignSystem_Process(t *testing.T) {
	w := newRBACWorld(t)
	if w.s.kb == nil {
		t.Skip("needs TEST_TYPESENSE_URL")
	}
	ra := newRestAs(t, w)
	admin := w.as(w.orgadmin)
	alice := w.as(w.alice)
	product := float64(w.s.projectID)

	// Before anything: no org design system, and how to get one.
	ctx0 := alice.call(t, "amp_design_context", map[string]any{"project_id": product})
	if !strings.Contains(strings.Join(toStrings(ctx0["next"]), " "), "amp_design_system_start") || ctx0["org_design_system"] != nil {
		t.Fatalf("no-org context: %v", ctx0)
	}

	// Only org owners and admins start the org's design system.
	wantErr(t, alice.callExpectError(t, "amp_design_system_start", map[string]any{"scope": "org", "project_id": product}), "owners and admins")
	// The project layer waits for the org's.
	wantErr(t, alice.callExpectError(t, "amp_design_system_start", map[string]any{"scope": "project", "project_id": product}), "hasn't published")

	start := admin.call(t, "amp_design_system_start", map[string]any{"scope": "org", "project_id": product, "team_id": float64(w.core.ID),
		"code": fmt.Sprintf("D%d", w.org.ID%100000), "brief": "First version."})["result"].(map[string]any)
	dsProject := intField(t, start["project"].(map[string]any), "id")
	if start["created_project"] != true || intField(t, start, "version") != 1 {
		t.Fatalf("start: %v", start)
	}
	tickets := map[string]int{}
	for _, tk := range start["tickets"].([]any) {
		m := tk.(map[string]any)
		tickets[m["ref"].(string)] = intField(t, m, "id")
	}
	for _, ph := range []string{"audit", "direction", "tokens", "components", "patterns", "publish"} {
		if tickets[ph] == 0 {
			t.Fatalf("template is missing phase %s: %v", ph, tickets)
		}
	}
	if st := getTask(t, admin, tickets["direction"])["state"]; st != "blocked" {
		t.Fatalf("direction should wait on the audit, is %v", st)
	}
	// Starting again resumes the open version.
	again := admin.call(t, "amp_design_system_start", map[string]any{"scope": "org", "project_id": product})["result"].(map[string]any)
	if again["already_open"] != true || intField(t, again, "epic_id") != intField(t, start, "epic_id") {
		t.Fatalf("restart: %v", again)
	}

	// design-system/ docs can't be written directly, over MCP or REST.
	wantErr(t, admin.callExpectError(t, "amp_kb_write", map[string]any{"scope": "org", "project_id": product, "path": "design-system/tokens.md",
		"title": "x", "content": "x", "tags": []any{}, "reason": "trying to skip the design system process here"}), "amp_design_system_publish")
	ra.want(t, 409, "orgadmin", http.MethodPost, fmt.Sprintf("/api/kb/doc?project_id=%d", dsProject),
		map[string]any{"path": "design-system/README.md", "title": "x", "content": "x"})

	docs := map[string]any{"README.md": "# Acme design system\n\nCalm, dense, precise.", "tokens.md": "# Tokens\n--accent: #0f766e",
		"components.md": "# Components", "patterns.md": "# Patterns", "voice.md": "# Voice"}
	// Not before every phase is approved.
	wantErr(t, admin.callExpectError(t, "amp_design_system_publish", map[string]any{"project_id": float64(dsProject), "docs": docs, "changelog": "v1"}), "approved every phase")

	approve := func(s *mcpStack, who string, ticket int, title string) int {
		a := s.call(t, "amp_publish_artifact", map[string]any{"task_id": float64(ticket), "title": title, "kind": "markdown", "content": "# " + title})
		id := intField(t, a["artifact"].(map[string]any), "id")
		ra.want(t, 200, who, http.MethodPost, fmt.Sprintf("/api/artifacts/%d/review", id), map[string]any{"approve": true})
		return id
	}
	var arts []int
	for _, ph := range []string{"audit", "direction", "tokens", "components"} {
		arts = append(arts, approve(admin, "orgadmin", tickets[ph], ph))
	}
	// Patterns has an artifact, but it isn't approved: still refused, and it says which.
	admin.call(t, "amp_publish_artifact", map[string]any{"task_id": float64(tickets["patterns"]), "title": "patterns", "kind": "markdown", "content": "# p"})
	wantErr(t, admin.callExpectError(t, "amp_design_system_publish", map[string]any{"project_id": float64(dsProject), "docs": docs, "changelog": "v1"}), "patterns")
	arts = append(arts, approve(admin, "orgadmin", tickets["patterns"], "patterns v2"))

	// Docs are checked; then it publishes.
	wantErr(t, admin.callExpectError(t, "amp_design_system_publish", map[string]any{"project_id": float64(dsProject),
		"docs": map[string]any{"README.md": "x"}, "changelog": "v1"}), "tokens.md")
	pub := admin.call(t, "amp_design_system_publish", map[string]any{"project_id": float64(dsProject), "docs": docs, "changelog": "First version."})
	ds := pub["design_system"].(map[string]any)
	if intField(t, ds, "version") != 1 || ds["building"] != false {
		t.Fatalf("published: %v", ds)
	}
	for _, id := range arts {
		if a := ra.want(t, 200, "orgadmin", http.MethodGet, fmt.Sprintf("/api/artifacts/%d", id), nil)["artifact"].(map[string]any); a["keep"] != true {
			t.Fatalf("approved artifact %d should be kept after publishing: %v", id, a)
		}
	}

	// Every product project now sees it, stamped.
	ctx1 := alice.call(t, "amp_design_context", map[string]any{"project_id": product})
	org := ctx1["org"].(map[string]any)
	readme := org["readme"].(string)
	if !strings.Contains(readme, "Design system v1") || !strings.Contains(readme, "Calm, dense, precise") || !strings.Contains(readme, "## Changelog") ||
		!strings.Contains(readme, "## Approved artifacts") {
		t.Fatalf("stamped readme: %s", readme)
	}
	if len(org["docs"].([]any)) != 5 || intField(t, ctx1["org_design_system"].(map[string]any), "version") != 1 {
		t.Fatalf("org layer: %v / %v", org["docs"], ctx1["org_design_system"])
	}

	// The project layer: same process, on top.
	ps := alice.call(t, "amp_design_system_start", map[string]any{"scope": "project", "project_id": product})["result"].(map[string]any)
	pt := map[string]int{}
	for _, tk := range ps["tickets"].([]any) {
		m := tk.(map[string]any)
		pt[m["ref"].(string)] = intField(t, m, "id")
	}
	if pt["delta"] == 0 || pt["additions"] == 0 || pt["publish"] == 0 {
		t.Fatalf("project template: %v", pt)
	}
	pdocs := map[string]any{"README.md": "# Storefront layer\nExtends Acme v1: promotions accent."}
	wantErr(t, alice.callExpectError(t, "amp_design_system_publish", map[string]any{"project_id": product, "docs": pdocs, "changelog": "v1"}), "approved every phase")
	approve(alice, "alice", pt["delta"], "delta")
	approve(alice, "alice", pt["additions"], "additions")
	alice.call(t, "amp_design_system_publish", map[string]any{"project_id": product, "docs": pdocs, "changelog": "Promotions accent."})
	ctx2 := alice.call(t, "amp_design_context", map[string]any{"project_id": product})
	proj := ctx2["project"].(map[string]any)
	if !strings.Contains(proj["readme"].(string), "extends the") || intField(t, ctx2["project_design_system"].(map[string]any), "version") != 1 {
		t.Fatalf("project layer: %v", proj)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
