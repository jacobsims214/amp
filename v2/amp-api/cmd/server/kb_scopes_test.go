package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/repository"
)

// Shared knowledge bases (docs/kb-scopes-design.md), against the rbacWorld:
//
//	org Acme: project linked to Core (owner: owner), Devs (member: alice,
//	bob, lead), Ops (member: carol); orgadmin owns the org but is on no
//	team; dave is an org member on no team. outsider is in org Other.

const goodReason = "Applies to every project the team runs; the user asked to share it."

// kbAllowed asserts a KB call passes access checks: it succeeds, or (with
// no Typesense configured) gets as far as "not configured".
func kbAllowed(t *testing.T, s *mcpStack, tool string, args map[string]interface{}) {
	t.Helper()
	if s.kb != nil {
		s.call(t, tool, args)
		return
	}
	wantErr(t, s.callExpectError(t, tool, args), "not configured")
}

func kbArgs(base map[string]interface{}, kv ...interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func TestKB_ScopeAccess(t *testing.T) {
	w := newRBACWorld(t)
	pid := float64(w.s.projectID)
	inProject := map[string]interface{}{"project_id": pid}
	org := kbArgs(inProject, "scope", "org")
	team := kbArgs(inProject, "scope", "team")
	devs := map[string]interface{}{"scope": "team", "team_id": float64(w.devs.ID)}
	doc := func(base map[string]interface{}, extra ...interface{}) map[string]interface{} {
		return kbArgs(base, append([]interface{}{"path", "p.md", "title", "t", "content", "c", "tags", []interface{}{"x"}}, extra...)...)
	}

	// Project KB: unchanged. Members read and write; outsiders can't see it.
	kbAllowed(t, w.as(w.alice), "amp_kb_list", inProject)
	kbAllowed(t, w.as(w.alice), "amp_kb_write", doc(inProject))
	wantErr(t, w.as(w.outsider).callExpectError(t, "amp_kb_list", inProject), "not found")

	// Org KB: every org member reads; only owners and admins write.
	kbAllowed(t, w.as(w.alice), "amp_kb_list", org)
	kbAllowed(t, w.as(w.dave), "amp_kb_list", map[string]interface{}{"scope": "org", "org_id": float64(w.org.ID)})
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_kb_write", doc(org, "reason", goodReason)), "only org owners and admins", "annotate")
	wantErr(t, w.as(w.lead).callExpectError(t, "amp_kb_delete", kbArgs(org, "path", "p.md")), "only org owners and admins")
	kbAllowed(t, w.as(w.orgadmin), "amp_kb_write", doc(map[string]interface{}{"scope": "org", "org_id": float64(w.org.ID)}, "reason", goodReason))
	wantErr(t, w.as(w.outsider).callExpectError(t, "amp_kb_list", map[string]interface{}{"scope": "org", "org_id": float64(w.org.ID)}), "not found")

	// Reindex needs write access to the KB it names, like amp_kb_write.
	kbAllowed(t, w.as(w.alice), "amp_kb_reindex", inProject)
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_kb_reindex", org), "only org owners and admins")
	kbAllowed(t, w.as(w.orgadmin), "amp_kb_reindex", map[string]interface{}{"scope": "org", "org_id": float64(w.org.ID)})
	wantErr(t, w.as(w.outsider).callExpectError(t, "amp_kb_reindex", inProject), "not found")

	// Team KB: "the team" for a project is the one of its teams you're on.
	kbAllowed(t, w.as(w.alice), "amp_kb_write", doc(team, "reason", goodReason))
	kbAllowed(t, w.as(w.carol), "amp_kb_list", team) // Ops
	kbAllowed(t, w.as(w.lead), "amp_kb_list", devs)
	// Team members only: not org admins, not other teams.
	wantErr(t, w.as(w.orgadmin).callExpectError(t, "amp_kb_list", team), "not on any of project", "Devs", "Ops", "Core")
	wantErr(t, w.as(w.orgadmin).callExpectError(t, "amp_kb_list", devs), "for its members")
	wantErr(t, w.as(w.carol).callExpectError(t, "amp_kb_list", devs), "for its members")
	wantErr(t, w.as(w.dave).callExpectError(t, "amp_kb_list", devs), "for its members")
	wantErr(t, w.as(w.outsider).callExpectError(t, "amp_kb_list", devs), "not found")
	// Global admins can reach any KB.
	kbAllowed(t, w.as(w.root), "amp_kb_list", devs)

	// On two of the project's teams: refuse to guess, and say which.
	if err := w.s.repo.SetTeamMember(context.Background(), w.ops.ID, w.bob.User.ID, "member"); err != nil {
		t.Fatal(err)
	}
	wantErr(t, w.as(w.bob).callExpectError(t, "amp_kb_list", team), "several of your teams", "Devs", "Ops", "pass team_id")
	kbAllowed(t, w.as(w.bob), "amp_kb_list", kbArgs(team, "team_id", float64(w.ops.ID)))

	// Never across orgs, even for someone in both.
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_kb_list", kbArgs(org, "org_id", float64(w.other.ID))), "isn't project")
	wantErr(t, w.as(w.root).callExpectError(t, "amp_kb_list", kbArgs(team, "team_id", float64(w.x.ID))), "different org")

	// Placing knowledge in a team or org KB needs a real reason.
	for _, r := range []interface{}{nil, "", "because"} {
		args := doc(team)
		if r != nil {
			args["reason"] = r
		}
		wantErr(t, w.as(w.alice).callExpectError(t, "amp_kb_write", args), "needs a reason", "write it to the project KB")
	}
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_kb_write", doc(map[string]interface{}{})), "name a knowledge base")
}

func TestKB_Sources(t *testing.T) {
	w := newRBACWorld(t)
	res := w.as(w.alice).call(t, "amp_kb_sources", map[string]interface{}{"project_id": float64(w.s.projectID)})
	srcs := res["sources"].([]interface{})
	type row struct {
		kind, name, role string
		read, write      bool
	}
	var got []row
	for _, s := range srcs {
		m := s.(map[string]interface{})
		role, _ := m["team_role"].(string)
		got = append(got, row{m["scope"].(string), m["name"].(string), role, m["can_read"] == true, m["can_write"] == true})
	}
	proj, err := w.s.repo.GetProject(context.Background(), w.s.projectID)
	if err != nil {
		t.Fatal(err)
	}
	want := []row{
		{"project", "project " + proj.Name, "", true, true},
		{"org", w.org.Name, "", true, false},
		{"team", "Core", "owner", false, false}, // the owner team first; alice isn't on it
		{"team", "Devs", "member", true, true},
		{"team", "Ops", "member", false, false},
		{"global", "AMP guide", "", true, false}, // the AMP guide, for everyone
	}
	if len(got) != len(want) {
		t.Fatalf("sources = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("source %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Teams you aren't on show no contents.
	for _, s := range srcs {
		m := s.(map[string]interface{})
		if m["can_read"] != true && m["docs"] != nil {
			t.Errorf("unreadable KB %v reports a doc count", m["name"])
		}
	}
	wantErr(t, w.as(w.outsider).callExpectError(t, "amp_kb_sources", map[string]interface{}{"project_id": float64(w.s.projectID)}), "not found")
}

// With a real Typesense: documents stay in the KB they were written to.
func TestKB_ScopesKeepDocumentsApart(t *testing.T) {
	w := newRBACWorld(t)
	if w.s.kb == nil {
		t.Skip("set TEST_TYPESENSE_URL to run against Typesense")
	}
	ctx := context.Background()
	pid := float64(w.s.projectID)
	write := func(id *auth.Identity, args map[string]interface{}, path, content string) {
		t.Helper()
		w.as(id).call(t, "amp_kb_write", kbArgs(args, "path", path, "title", path, "content", content, "tags", []interface{}{"scoped"}))
	}
	paths := func(id *auth.Identity, args map[string]interface{}) []string {
		t.Helper()
		var out []string
		for _, d := range w.as(id).call(t, "amp_kb_list", args)["docs"].([]interface{}) {
			out = append(out, d.(map[string]interface{})["path"].(string))
		}
		return out
	}
	project := map[string]interface{}{"project_id": pid}
	team := map[string]interface{}{"project_id": pid, "scope": "team"}
	org := map[string]interface{}{"scope": "org", "org_id": float64(w.org.ID)}

	write(w.alice, project, "project/notes.md", "Project-only notes about the payments service.")
	write(w.alice, kbArgs(team, "reason", goodReason), "team/deploy-runbook.md", "How the Devs team deploys: blue-green rollout.")
	write(w.orgadmin, kbArgs(org, "reason", "Brand rules apply to every product in Acme; requested by the design lead."), "org/brand.md", "Acme brand colours: indigo and slate.")

	if got := paths(w.alice, project); len(got) != 1 || got[0] != "project/notes.md" {
		t.Fatalf("project KB = %v", got)
	}
	if got := paths(w.lead, map[string]interface{}{"scope": "team", "team_id": float64(w.devs.ID)}); len(got) != 1 || got[0] != "team/deploy-runbook.md" {
		t.Fatalf("Devs KB as the lead = %v", got)
	}
	if got := paths(w.carol, kbArgs(project, "scope", "org")); len(got) != 1 || got[0] != "org/brand.md" {
		t.Fatalf("org KB as carol = %v", got)
	}
	// Search is per KB: the project search doesn't surface team or org docs.
	for _, q := range []string{"blue-green", "indigo"} {
		res := w.as(w.alice).call(t, "amp_kb_search", map[string]interface{}{"project_id": pid, "query": q})
		if res["count"].(float64) != 0 {
			t.Errorf("project search for %q found %v", q, res["results"])
		}
	}
	res := w.as(w.alice).call(t, "amp_kb_search", kbArgs(team, "query", "blue-green"))
	if res["count"].(float64) != 1 {
		t.Fatalf("team search = %v", res)
	}
	// The placement reason travels with the doc.
	got := w.as(w.alice).call(t, "amp_kb_get", kbArgs(team, "path", "team/deploy-runbook.md"))
	if got["reason"] != goodReason || got["scope"] != "team" {
		t.Fatalf("team doc = %v", got)
	}
	// Anyone in the org can annotate an org doc, but not change it.
	w.as(w.alice).call(t, "amp_kb_annotate", kbArgs(org, "path", "org/brand.md", "text", "The slate shade changed in v3."))
	brand := w.as(w.dave).call(t, "amp_kb_get", kbArgs(org, "path", "org/brand.md"))
	if anns, _ := brand["annotations"].([]interface{}); len(anns) != 1 || brand["reason"] == "" {
		t.Fatalf("org doc after annotation = %v", brand)
	}
	wantErr(t, w.as(w.alice).callExpectError(t, "amp_kb_delete", kbArgs(org, "path", "org/brand.md")), "only org owners and admins")

	// Deleting a team deletes its KB.
	if n, _ := w.s.kb.CountDocs(ctx, kb.Team(w.devs.ID)); n != 1 {
		t.Fatalf("Devs KB has %d docs before delete", n)
	}
	w.as(w.orgadmin).call(t, "amp_manage_teams", map[string]interface{}{"action": "delete", "team_id": float64(w.devs.ID)})
	if n, _ := w.s.kb.CountDocs(ctx, kb.Team(w.devs.ID)); n != 0 {
		t.Fatalf("Devs KB still has %d docs after the team was deleted", n)
	}
	// ...into the trash: the docs stay in Postgres, soft-deleted, for 30
	// days, and the KB can be restored from there.
	devsKB := repository.KBScope{Kind: "team", ID: w.devs.ID}
	if trashed, err := w.s.repo.ListTrashedKBPaths(ctx, devsKB, "kb-drop"); err != nil || len(trashed) != 1 || trashed[0] != "team/deploy-runbook.md" {
		t.Fatalf("Devs KB trash after the team was deleted = %v, %v", trashed, err)
	}
	if n, err := w.s.kb.RestoreKB(ctx, kb.Team(w.devs.ID), "root"); err != nil || n != 1 {
		t.Fatalf("RestoreKB = %d, %v", n, err)
	}
	if hits, err := w.s.kb.Search(ctx, kb.Team(w.devs.ID), "blue-green", nil, 10, 0, 0); err != nil || len(hits) != 1 {
		t.Fatalf("search of the restored Devs KB = %v, %v", hits, err)
	}
	w.s.kb.Drop(ctx, kb.Team(w.devs.ID)) //nolint:errcheck // clean up the restored index

	// Archiving a project is not deleting it: its KB stays live (not in the
	// trash, so the purge never takes it) and searchable after a restore.
	w.as(w.root).call(t, "amp_archive_project", project)
	if _, err := w.s.repo.GetKBDocument(ctx, repository.KBScope{Kind: "project", ID: w.s.projectID}, "project/notes.md"); err != nil {
		t.Fatalf("project KB doc after archive: %v", err)
	}
	w.as(w.root).call(t, "amp_restore_project", project)
	// (as root: alice reached the project through the deleted Devs team)
	res = w.as(w.root).call(t, "amp_kb_search", map[string]interface{}{"project_id": pid, "query": "payments"})
	if res["count"].(float64) != 1 {
		t.Fatalf("project search after archive and restore = %v", res)
	}
}

func TestREST_KBScopesAndProjectSettings(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	team := fmt.Sprintf("team_id=%d", w.devs.ID)
	org := fmt.Sprintf("org_id=%d", w.org.ID)
	ok := http.StatusOK
	if w.s.kb == nil {
		ok = http.StatusServiceUnavailable // access checks pass, then "not configured"
	}

	info := ra.want(t, 200, "alice", "GET", "/api/kb/info?"+team, nil)
	if info["scope"] != "team" || info["can_write"] != true {
		t.Fatalf("team KB info = %v", info)
	}
	if info := ra.want(t, 200, "alice", "GET", "/api/kb/info?"+org, nil); info["can_write"] != false || info["can_read"] != true {
		t.Fatalf("org KB info for a member = %v", info)
	}
	ra.want(t, ok, "alice", "GET", "/api/kb/list?"+team, nil)
	ra.want(t, 403, "carol", "GET", "/api/kb/list?"+team, nil)
	ra.want(t, 403, "alice", "POST", "/api/kb/doc?"+org, map[string]any{"path": "x.md", "title": "x", "content": "x"})
	ra.want(t, 403, "alice", "DELETE", "/api/kb/doc?"+org+"&path=x.md", nil)
	ra.want(t, ok, "orgadmin", "POST", "/api/kb/doc?"+org, map[string]any{"path": "x.md", "title": "x", "content": "x", "reason": "org-wide"})
	ra.want(t, 404, "outsider", "GET", "/api/kb/list?"+org, nil)
	ra.want(t, 400, "alice", "GET", "/api/kb/list", nil)
	ra.want(t, 400, "root", "GET", "/api/kb/config?"+team, nil)
	ra.want(t, 403, "lead", "GET", "/api/kb/config?"+team, nil)

	// Project settings: owners rename and describe the project.
	proj := fmt.Sprintf("/api/projects/%d", w.s.projectID)
	ra.want(t, 403, "alice", "PATCH", proj, map[string]any{"name": "nope"})
	ra.want(t, 400, "owner", "PATCH", proj, map[string]any{"name": "  "})
	p := ra.want(t, 200, "owner", "PATCH", proj, map[string]any{"name": "Renamed", "description": "Now with a description"})["project"].(map[string]any)
	if p["name"] != "Renamed" || p["description"] != "Now with a description" || p["open_reassign"] != true {
		t.Fatalf("PATCH project = %v", p)
	}
}
