package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/service"
)

func refsOf(items []interface{}) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.(map[string]interface{})["ref"].(string))
	}
	return out
}

// A day of alice's work across two orgs' projects, reported, saved, and read
// by people who can and can't see each project.
func TestDayReports(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	repo := w.s.repo

	// alice is also on team X in org Other, which owns a second project.
	if err := repo.SetTeamMember(ctx, w.x.ID, w.alice.User.ID, domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	sfx := time.Now().UnixNano()
	other, err := repo.CreateProject(ctx, domain.CreateProjectRequest{Name: fmt.Sprintf("Other %d", sfx), Code: fmt.Sprintf("oth%d", sfx%100000), OrgID: &w.other.ID, TeamID: &w.x.ID})
	if err != nil {
		t.Fatal(err)
	}
	oe, _ := repo.CreateEpic(ctx, domain.CreateEpicRequest{ProjectID: other.ID, Name: "oe", Priority: "1"})
	os_, _ := repo.CreateStory(ctx, domain.CreateStoryRequest{ProjectID: other.ID, EpicID: oe.ID, Name: "os", Priority: "1"})

	alice := w.as(w.alice)
	mk := func(name string, project, epic, story int, extra map[string]interface{}) int {
		args := map[string]interface{}{"project_id": float64(project), "epic_id": float64(epic), "story_id": float64(story),
			"name": name, "description": "d", "acceptance_criteria": "a", "assigned_to": "amp-worker"}
		for k, v := range extra {
			args[k] = v
		}
		return taskID(t, alice.call(t, "amp_create_task", args))
	}
	acme := w.s
	done := mk("ship login", acme.projectID, acme.epicID, acme.storyID, map[string]interface{}{"metadata": map[string]interface{}{"jira_key": "PLAT-1"}})
	running := mk("build search", acme.projectID, acme.epicID, acme.storyID, nil)
	stuck := mk("fix flaky test", acme.projectID, acme.epicID, acme.storyID, nil)
	created := mk("plan billing", acme.projectID, acme.epicID, acme.storyID, nil)
	elsewhere := mk("other org work", other.ID, oe.ID, os_.ID, nil)
	bobs := taskID(t, w.as(w.bob).mkTask(t, "bob's ticket", nil))

	alice.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(done), "agent_id": "a"})
	alice.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(done)})
	alice.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(running), "agent_id": "a"})
	alice.call(t, "amp_block_task", map[string]interface{}{"task_id": float64(stuck), "reason": "waiting on API keys"})
	alice.call(t, "amp_complete_task", map[string]interface{}{"task_id": float64(elsewhere)})
	alice.call(t, "amp_add_task_comment", map[string]interface{}{"task_id": float64(bobs), "body": "reviewed, looks good"})

	ref := func(code string, id int) string { return domain.TaskRef(code, id) }
	acmeCode := func() string { p, _ := repo.GetProject(ctx, acme.projectID); return p.Code }()

	// The draft: each ticket once, in the right section.
	draft := alice.call(t, "amp_day_report", map[string]interface{}{"tz": "UTC"})["report"].(map[string]interface{})
	data := draft["data"].(map[string]interface{})
	check := func(section string, want ...string) {
		t.Helper()
		got := refsOf(data[section].([]interface{}))
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", section, got, want)
		}
	}
	check("completed", ref(acmeCode, done), ref(other.Code, elsewhere))
	check("in_progress", ref(acmeCode, running))
	check("blocked", ref(acmeCode, stuck))
	check("started")
	check("created", ref(acmeCode, created))
	check("touched", ref(acmeCode, bobs))
	if draft["saved"] != false {
		t.Fatal("a draft isn't saved")
	}
	first := data["completed"].([]interface{})[0].(map[string]interface{})
	if first["ref"] == ref(acmeCode, done) && first["metadata"].(map[string]interface{})["jira_key"] != "PLAT-1" {
		t.Errorf("report items carry effective metadata: %v", first)
	}
	if st := data["stats"].(map[string]interface{}); st["completed"] != float64(2) || st["projects"] != float64(2) || st["blocked"] != float64(1) {
		t.Errorf("stats = %v", st)
	}

	// Save it with a summary; it's then listed, with markdown ready to paste.
	saved := alice.call(t, "amp_save_day_report", map[string]interface{}{"tz": "UTC", "summary": "Shipped login. Search is underway; waiting on API keys for the flaky test."})["report"].(map[string]interface{})
	md := saved["markdown"].(string)
	for _, want := range []string{"# Daily report:", "Shipped login.", "## Completed (2)", ref(acmeCode, done) + " ship login", "blocked: waiting on API keys", "## Also worked on (1)"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	again := alice.call(t, "amp_day_report", map[string]interface{}{"tz": "UTC"})["report"].(map[string]interface{})
	if again["saved"] != true || !strings.HasPrefix(again["summary"].(string), "Shipped login") {
		t.Errorf("draft after saving keeps the summary: %v", again["summary"])
	}

	// Who can read it, and what they see.
	readAs := func(id *auth.Identity) map[string]interface{} {
		t.Helper()
		res := w.as(id).call(t, "amp_list_day_reports", map[string]interface{}{"user": w.alice.Email, "tz": "UTC"})
		reports := res["reports"].([]interface{})
		if len(reports) != 1 {
			t.Fatalf("%s sees %d reports", id.Name, len(reports))
		}
		return reports[0].(map[string]interface{})
	}
	// carol shares Acme: sees Acme tickets; the Other org's ticket is hidden.
	carols := readAs(w.carol)
	if got := refsOf(carols["data"].(map[string]interface{})["completed"].([]interface{})); !slices.Equal(got, []string{ref(acmeCode, done)}) || carols["hidden_items"] != float64(1) {
		t.Errorf("carol's view: completed %v, hidden %v", got, carols["hidden_items"])
	}
	if !strings.Contains(carols["markdown"].(string), "Shipped login.") || !strings.Contains(carols["markdown"].(string), "1 more ticket in projects you can't see") {
		t.Errorf("carol's markdown:\n%s", carols["markdown"])
	}
	// outsider shares only the Other org: sees only that ticket.
	outs := readAs(w.outsider)
	if got := refsOf(outs["data"].(map[string]interface{})["completed"].([]interface{})); !slices.Equal(got, []string{ref(other.Code, elsewhere)}) {
		t.Errorf("outsider's view: completed %v", got)
	}
	// dave is in Acme but on no project: summary only.
	daves := readAs(w.dave)
	if daves["hidden_items"] != float64(6) || daves["summary"] == "" {
		t.Errorf("dave's view: hidden %v, summary %q", daves["hidden_items"], daves["summary"])
	}
	// Someone in none of alice's orgs can't read it.
	loner, err := repo.UpsertUserFromClaims(ctx, fmt.Sprintf("sub-loner-%d", sfx), fmt.Sprintf("loner-%d@rbac.test", sfx), "loner", nil)
	if err != nil {
		t.Fatal(err)
	}
	lonerID := &auth.Identity{Subject: loner.Subject, Email: loner.Email, Name: "loner", User: loner}
	wantErr(t, w.as(lonerID).callExpectError(t, "amp_list_day_reports", map[string]interface{}{"user": w.alice.Email}), "no reports for")
	wantErr(t, w.as(lonerID).callExpectError(t, "amp_velocity", map[string]interface{}{"user": w.alice.Email}), "no reports for")

	// Velocity: today's two completions, by project; carol counts only Acme.
	vel := alice.call(t, "amp_velocity", map[string]interface{}{"tz": "UTC"})
	days := vel["days"].([]interface{})
	if len(days) != 7 || vel["total_completed"] != float64(2) {
		t.Fatalf("velocity = %v", vel)
	}
	today := days[len(days)-1].(map[string]interface{})
	if today["completed"] != float64(2) || today["by_project"].(map[string]interface{})[other.Code] != float64(1) {
		t.Errorf("today = %v", today)
	}
	if v := w.as(w.carol).call(t, "amp_velocity", map[string]interface{}{"user": w.alice.Email, "tz": "UTC"}); v["total_completed"] != float64(1) {
		t.Errorf("carol's view of alice's velocity = %v", v["total_completed"])
	}

	// Input checks.
	wantErr(t, alice.callExpectError(t, "amp_day_report", map[string]interface{}{"tz": "Mars/Olympus"}), "unknown time zone")
	wantErr(t, alice.callExpectError(t, "amp_day_report", map[string]interface{}{"date": "16/10/2026"}), "YYYY-MM-DD")
	wantErr(t, alice.callExpectError(t, "amp_list_day_reports", map[string]interface{}{"from": "2025-01-01", "to": "2026-01-01"}), "at most")

	// REST, for the UI.
	ra := newRestAs(t, w)
	day, _, _, _ := service.DayWindow("", "UTC")
	ra.want(t, http.StatusOK, "alice", "PUT", "/api/day-reports/"+day+"?tz=UTC", map[string]any{"summary": "Edited in the UI."})
	got := ra.want(t, http.StatusOK, "carol", "GET", "/api/day-reports?tz=UTC&user="+w.alice.Email, nil)["reports"].([]any)
	if len(got) != 1 || got[0].(map[string]any)["summary"] != "Edited in the UI." {
		t.Fatalf("REST reports = %v", got)
	}
	people := ra.want(t, http.StatusOK, "carol", "GET", "/api/day-reports/people", nil)["people"].([]any)
	var emails []string
	for _, p := range people {
		emails = append(emails, p.(map[string]any)["email"].(string))
	}
	if !slices.Contains(emails, w.alice.Email) || slices.Contains(emails, w.outsider.Email) {
		t.Errorf("carol's report people = %v", emails)
	}
	ra.want(t, http.StatusOK, "alice", "GET", "/api/velocity?tz=UTC", nil)
	ra.want(t, http.StatusOK, "alice", "GET", "/api/day-reports/draft?tz=UTC", nil)
}

// Days are local: they start at midnight in the given zone, and a
// daylight-saving day is 23 or 25 hours long.
func TestDayWindow(t *testing.T) {
	for _, c := range []struct {
		date, tz string
		hours    float64
		startUTC string
	}{
		{"2026-03-08", "America/New_York", 23, "2026-03-08T05:00:00Z"},
		{"2026-11-01", "America/New_York", 25, "2026-11-01T04:00:00Z"},
		{"2026-10-16", "", 24, "2026-10-16T00:00:00Z"},
		{"2026-10-16", "Asia/Kolkata", 24, "2026-10-15T18:30:00Z"},
	} {
		day, start, end, err := service.DayWindow(c.date, c.tz)
		if err != nil || day != c.date || end.Sub(start).Hours() != c.hours || start.UTC().Format(time.RFC3339) != c.startUTC {
			t.Errorf("%s %s: day %s, %v → %v (%v hours), err %v", c.date, c.tz, day, start.UTC(), end.UTC(), end.Sub(start).Hours(), err)
		}
	}
}
