package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
)

// Every tool AMP has ever listed. Superseded ones are hidden from tools/list
// but must stay callable: prompts, skills and clients name them.
var everyToolName = strings.Fields(`amp_add_task_comment amp_answer_question amp_approve_plan amp_archive_project
amp_assign_task amp_audit_log amp_block_task amp_check_plan amp_claim_task amp_complete_task amp_create_epic
amp_create_org amp_create_project amp_create_story amp_create_task amp_create_tasks amp_day_report amp_delete_epic
amp_delete_story amp_delete_task amp_design_context amp_design_system_publish amp_design_system_start
amp_dispatch_task amp_export_project amp_find_by_metadata amp_get_artifact amp_get_epic amp_get_project
amp_get_story amp_get_task amp_get_task_comments amp_get_task_runs amp_get_ticket_history amp_guide
amp_handover_tasks amp_import_project amp_kb_annotate amp_kb_delete amp_kb_get amp_kb_list amp_kb_reindex
amp_kb_resolve_annotation amp_kb_search amp_kb_sources amp_kb_status amp_kb_tags amp_kb_write amp_keep_artifact
amp_list_artifacts amp_list_day_reports amp_list_deleted_tasks amp_list_epics amp_list_orgs
amp_list_project_members amp_list_project_stories amp_list_projects amp_list_stories amp_list_tasks
amp_list_unmerged amp_manage_invites amp_manage_org_members amp_manage_project_teams amp_manage_team_members
amp_manage_teams amp_move_story amp_move_task amp_project_day_reports amp_publish_artifact amp_report_run
amp_reset_project amp_restore_project amp_restore_tasks amp_review_artifact amp_save_day_report
amp_set_task_dependencies amp_set_task_start_at amp_set_task_state amp_trace amp_update_epic
amp_update_project_settings amp_update_story amp_update_task amp_velocity amp_whoami amp_get_plan`)

type listedTool struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	InputSchema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	} `json:"inputSchema"`
	Annotations struct {
		ReadOnly    *bool `json:"readOnlyHint"`
		Destructive *bool `json:"destructiveHint"`
	} `json:"annotations"`
}

func (s *mcpStack) listTools(t *testing.T) []listedTool {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	b, _ := json.Marshal(s.srv.HandleMessage(s.callCtx(), raw))
	var resp struct {
		Result struct{ Tools []listedTool } `json:"result"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Result.Tools
}

func names(ts []listedTool) map[string]bool {
	m := map[string]bool{}
	for _, t := range ts {
		m[t.Name] = true
	}
	return m
}

func TestCatalog_EveryToolStillCallable(t *testing.T) {
	s := setupMCP(t)
	for _, name := range everyToolName {
		// Missing arguments: each tool must answer (an argument error is
		// fine) rather than the protocol's "tool not found".
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": map[string]any{}}})
		b, _ := json.Marshal(s.srv.HandleMessage(s.callCtx(), raw))
		var resp struct {
			Error *struct{ Message string } `json:"error"`
		}
		_ = json.Unmarshal(b, &resp)
		if resp.Error != nil && strings.Contains(resp.Error.Message, "not found: tool not found") {
			t.Errorf("%s: %s", name, resp.Error.Message)
		}
	}
}

func TestCatalog_ListedToolsPerCaller(t *testing.T) {
	w := newRBACWorld(t)
	hidden := []string{"amp_get_ticket_history", "amp_get_task_comments", "amp_get_task_runs", "amp_create_task",
		"amp_claim_task", "amp_list_stories", "amp_list_project_stories", "amp_get_epic", "amp_get_story",
		"amp_archive_project", "amp_restore_project", "amp_reset_project", "amp_export_project", "amp_import_project",
		"amp_create_org", "amp_kb_tags", "amp_kb_reindex"}
	admin := []string{"amp_manage_invites", "amp_manage_org_members", "amp_manage_teams", "amp_audit_log"}
	person := []string{"amp_approve_plan", "amp_review_artifact", "amp_keep_artifact"}

	agentUser := *w.alice.User
	agentUser.Kind = domain.UserKindAgent
	agent := &auth.Identity{Subject: w.alice.Subject, Email: w.alice.Email, User: &agentUser}

	for _, c := range []struct {
		who            string
		id             *auth.Identity
		admin, persons bool
	}{
		{"member", w.alice, false, true},
		{"org admin", w.orgadmin, true, true},
		{"global admin", w.root, true, true},
		{"agent", agent, false, false},
	} {
		got := names(w.s.as(c.id).listTools(t))
		for _, n := range hidden {
			if got[n] {
				t.Errorf("%s: hidden alias %s is listed", c.who, n)
			}
		}
		for _, n := range admin {
			if got[n] != c.admin {
				t.Errorf("%s: %s listed=%v, want %v", c.who, n, got[n], c.admin)
			}
		}
		for _, n := range person {
			if got[n] != c.persons {
				t.Errorf("%s: %s listed=%v, want %v", c.who, n, got[n], c.persons)
			}
		}
		for _, n := range []string{"amp_get_task", "amp_get_plan", "amp_list_tasks", "amp_dispatch_task", "amp_create_tasks", "amp_manage_team_members"} {
			if !got[n] {
				t.Errorf("%s: %s not listed", c.who, n)
			}
		}
		t.Logf("%s sees %d tools", c.who, len(got))
	}
}

func TestCatalog_ToolsAreAnnotated(t *testing.T) {
	s := setupMCP(t)
	for _, tl := range s.listTools(t) {
		if tl.Title == "" || tl.Annotations.ReadOnly == nil || tl.Annotations.Destructive == nil {
			t.Errorf("%s has no title or annotations", tl.Name)
		}
	}
}

// opencode's Code Mode finds MCP tools by keyword search
// (packages/codemode/src/tool-runtime.ts). This ports its scoring so the
// words agents actually search with keep finding the right AMP tool.
func TestCatalog_SearchFindsTheRightTool(t *testing.T) {
	s := setupMCP(t)
	tools := s.listTools(t)
	camel := regexp.MustCompile(`([a-z0-9])([A-Z])`)
	split := regexp.MustCompile(`[^a-z0-9]+`)
	tokenize := func(q string) []string {
		var out []string
		for _, w := range split.Split(strings.ToLower(camel.ReplaceAllString(q, "$1 $2")), -1) {
			if w != "" && w != "*" {
				out = append(out, w)
			}
		}
		return out
	}
	forms := func(w string) []string {
		f := []string{w}
		if strings.HasSuffix(w, "es") && len(w) > 3 {
			f = append(f, w[:len(w)-2])
		}
		if strings.HasSuffix(w, "s") && len(w) > 2 {
			f = append(f, w[:len(w)-1])
		}
		return f
	}
	type entry struct {
		name, path, desc, text string
		words                  []string
	}
	var entries []entry
	for _, tl := range tools {
		path := "amp." + tl.Name
		parts := []string{path, tl.Description}
		for k, p := range tl.InputSchema.Properties {
			parts = append(parts, k, p.Description)
		}
		entries = append(entries, entry{tl.Name, path, strings.ToLower(tl.Description), strings.ToLower(strings.Join(parts, "\n")), tokenize(path)})
	}
	any := func(fs []string, ok func(string) bool) bool {
		for _, f := range fs {
			if ok(f) {
				return true
			}
		}
		return false
	}
	rank := func(q string) []string {
		type hit struct {
			n     string
			score int
		}
		var hits []hit
		for _, e := range entries {
			score := 0
			for _, w := range tokenize(q) {
				fs := forms(w)
				if any(fs, func(f string) bool { return e.path == f || strings.HasSuffix(e.path, "."+f) }) {
					score += 20
				}
				if any(fs, func(f string) bool {
					for _, pw := range e.words {
						if pw == f {
							return true
						}
					}
					return false
				}) {
					score += 12
				}
				if any(fs, func(f string) bool { return strings.Contains(e.path, f) }) {
					score += 8
				}
				if any(fs, func(f string) bool { return strings.Contains(e.desc, f) }) {
					score += 4
				}
				if any(fs, func(f string) bool { return strings.Contains(e.text, f) }) {
					score += 2
				}
			}
			if score > 0 {
				hits = append(hits, hit{e.name, score})
			}
		}
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].score != hits[j].score {
				return hits[i].score > hits[j].score
			}
			return hits[i].n < hits[j].n
		})
		out := make([]string, len(hits))
		for i, h := range hits {
			out[i] = h.n
		}
		return out
	}
	cases := [][2]string{
		{"comment on a ticket", "amp_add_task_comment"}, {"post progress update", "amp_add_task_comment"},
		{"ask the user a question", "amp_block_task"}, {"block ticket", "amp_block_task"},
		{"answer question", "amp_answer_question"}, {"review artifact", "amp_review_artifact"},
		{"approve mockup", "amp_review_artifact"}, {"publish mockup", "amp_publish_artifact"},
		{"upload image", "amp_publish_artifact"}, {"list my tickets", "amp_list_tasks"},
		{"what should I work on", "amp_list_tasks"}, {"search knowledge base", "amp_kb_search"},
		{"write kb doc", "amp_kb_write"}, {"add dependency", "amp_set_task_dependencies"},
		{"dispatch ticket", "amp_dispatch_task"}, {"start work on ticket", "amp_dispatch_task"},
		{"complete ticket", "amp_complete_task"}, {"finish task", "amp_complete_task"},
		{"report token usage", "amp_report_run"}, {"who am I", "amp_whoami"},
		{"design system", "amp_design_system_start"}, {"create tickets", "amp_create_tasks"},
		{"plan tickets with dependencies", "amp_create_tasks"}, {"delete ticket", "amp_delete_task"},
		{"move ticket to another story", "amp_move_task"}, {"project members", "amp_list_project_members"},
		{"standup", "amp_project_day_reports"}, {"restore deleted", "amp_restore_tasks"},
		{"get ticket", "amp_get_task"}, {"ticket history", "amp_get_task"},
		{"approve plan", "amp_approve_plan"}, {"check plan", "amp_check_plan"},
		{"amp task get list dispatch", "amp_get_task"}, {"reassign ticket", "amp_assign_task"},
		{"design context", "amp_design_context"}, {"how do I use amp", "amp_guide"},
		{"list epics", "amp_list_epics"}, {"list stories", "amp_get_plan|amp_list_epics"}, {"see the plan", "amp_get_plan"},
	}
	// Queries whose words are in other tools' names, which opencode scores
	// far above any description: the right tool still makes the first page.
	nameBound := map[string]bool{"post progress update": true, "start work on ticket": true,
		"plan tickets with dependencies": true}
	misses := 0
	for _, c := range cases {
		r := rank(c[0])
		pos := -1
		for i, n := range r {
			if strings.Contains("|"+c[1]+"|", "|"+n+"|") {
				pos = i + 1
				break
			}
		}
		if limit := map[bool]int{false: 3, true: 6}[nameBound[c[0]]]; pos < 1 || pos > limit {
			misses++
			top := r
			if len(top) > 4 {
				top = top[:4]
			}
			t.Errorf("search %q: %s is at %d, top %v", c[0], c[1], pos, top)
		}
	}
	t.Logf("%d of %d searches miss (top 3, or top 6 for %d name-bound queries)", misses, len(cases), len(nameBound))
}

// amp_get_task with include answers what amp_get_task_comments,
// amp_get_ticket_history and amp_get_task_runs did, and get_plan what the
// epic and story lists did.
func TestCatalog_ConsolidatedReads(t *testing.T) {
	s := setupMCP(t)
	id := intField(t, s.mkTask(t, "consolidated", nil), "id")
	for i := 0; i < 3; i++ {
		s.call(t, "amp_add_task_comment", map[string]any{"task_id": float64(id), "body": fmt.Sprintf("note %d", i)})
	}
	s.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(id), "agent_id": "w"})
	s.call(t, "amp_report_run", map[string]any{"task_id": float64(id), "model": "m", "input_tokens": float64(10)})

	full := s.call(t, "amp_get_task", map[string]any{"task_id": float64(id), "include": []any{"comments", "history", "runs"}})
	old := s.call(t, "amp_get_ticket_history", map[string]any{"task_id": float64(id)})
	for _, k := range []string{"comments", "history", "runs"} {
		if got, want := sliceLen(t, full, k), sliceLen(t, old, k); got != want || got == 0 {
			t.Errorf("include %s: %d entries, old tool %d", k, got, want)
		}
	}
	if _, ok := full["usage"]; !ok {
		t.Error("include runs: no usage totals")
	}
	last := s.call(t, "amp_get_task", map[string]any{"task_id": float64(id), "include": []any{"comments"}, "limit": float64(2)})
	if sliceLen(t, last, "comments") != 2 || intField(t, last, "comments_total") != 3 {
		t.Errorf("limit 2: %v", last)
	}
	if c := last["comments"].([]any); c[1].(map[string]any)["body"] != "note 2" {
		t.Errorf("limit keeps the most recent: %v", c)
	}
	plain := s.call(t, "amp_get_task", map[string]any{"task_id": float64(id)})
	if _, ok := plain["comments"]; ok {
		t.Error("no include: comments returned")
	}
	if msg := s.callExpectError(t, "amp_get_task", map[string]any{"task_id": float64(id), "include": []any{"bogus"}}); !strings.Contains(msg, "bogus") {
		t.Errorf("unknown include: %q", msg)
	}

	plan := s.call(t, "amp_get_plan", map[string]any{"project_id": float64(s.projectID)})
	epics := plan["epics"].([]any)
	if len(epics) != 1 {
		t.Fatalf("plan epics: %v", plan)
	}
	e := epics[0].(map[string]any)
	stories := e["stories"].([]any)
	if int(e["id"].(float64)) != s.epicID || len(stories) != 1 || int(stories[0].(map[string]any)["id"].(float64)) != s.storyID {
		t.Fatalf("plan tree: %v", e)
	}
	if n := stories[0].(map[string]any)["tickets"].(map[string]any)["in_progress"]; n != float64(1) {
		t.Errorf("story ticket counts: %v", stories[0])
	}
	if msg := s.callExpectError(t, "amp_get_plan", map[string]any{"project_id": float64(s.projectID), "epic_id": float64(999999)}); !strings.Contains(msg, "999999") {
		t.Errorf("unknown epic: %q", msg)
	}
}

// amp_list_tasks answers compactly unless asked for the full records:
// questions come with their text, completed tickets are counted but only
// listed on request.
func TestCatalog_ListTasksCompactByDefault(t *testing.T) {
	s := setupMCP(t)
	done := intField(t, s.mkTask(t, "done", nil), "id")
	asked := intField(t, s.mkTask(t, "asked", nil), "id")
	ready := intField(t, s.mkTask(t, "ready", nil), "id")
	s.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(done), "agent_id": "w"})
	s.call(t, "amp_complete_task", map[string]any{"task_id": float64(done)})
	s.call(t, "amp_dispatch_task", map[string]any{"task_id": float64(asked), "agent_id": "w"})
	s.call(t, "amp_block_task", map[string]any{"task_id": float64(asked), "reason": "Stripe or Avalara?"})

	lines := func(out map[string]any) map[int]bool {
		ids := map[int]bool{}
		for _, v := range out["by_epic"].(map[string]any) {
			for _, l := range v.([]any) {
				ids[int(l.(map[string]any)["id"].(float64))] = true
			}
		}
		return ids
	}
	out := s.call(t, "amp_list_tasks", map[string]any{"project_id": float64(s.projectID)})
	if r := out["ready_to_dispatch"].([]any); len(r) != 1 || int(r[0].(float64)) != ready {
		t.Errorf("ready: %v", out["ready_to_dispatch"])
	}
	q := out["questions"].([]any)
	if len(q) != 1 || q[0].(map[string]any)["question"] != "Stripe or Avalara?" || int(q[0].(map[string]any)["id"].(float64)) != asked {
		t.Errorf("questions: %v", q)
	}
	if ids := lines(out); ids[done] || !ids[asked] || !ids[ready] || out["completed"] != float64(1) {
		t.Errorf("by_epic %v, completed %v", ids, out["completed"])
	}
	if ids := lines(s.call(t, "amp_list_tasks", map[string]any{"project_id": float64(s.projectID), "include_completed": true})); !ids[done] {
		t.Errorf("include_completed: %v", ids)
	}
	full := s.call(t, "amp_list_tasks", map[string]any{"project_id": float64(s.projectID), "format": "full"})
	if sliceLen(t, full, "completed") != 1 || sliceLen(t, full, "blocked") != 1 {
		t.Errorf("full: %v", full)
	}
	if msg := s.callExpectError(t, "amp_list_tasks", map[string]any{"project_id": float64(s.projectID), "format": "xml"}); !strings.Contains(msg, "full") {
		t.Errorf("bad format: %q", msg)
	}
}
