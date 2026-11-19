package main

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// GET /api/projects/:id/last-activity returns, for each of the project's
// in_progress tickets, the time of its newest activity-log row (AMP #2545).
// Mission's stall alarm reads it instead of the activity feed, which #1586
// capped at 100 rows. Tickets in other states, other projects' tickets, and
// running tickets with no activity are absent. The route has the same access
// as /activity: any project member.
func TestRest_ProjectRunningLastActivity(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	ctx := context.Background()
	repo := w.s.repo

	path := func(project int) string { return fmt.Sprintf("/api/projects/%d/last-activity", project) }
	get := func(t *testing.T, who string, project int) map[int]time.Time {
		t.Helper()
		out := ra.want(t, 200, who, http.MethodGet, path(project), nil)
		if len(out) != 1 {
			t.Fatalf("GET %s as %s: keys %v, want just last_activity", path(project), who, out)
		}
		raw, ok := out["last_activity"].(map[string]any)
		if !ok {
			t.Fatalf("GET %s as %s: last_activity is %T, want an object", path(project), who, out["last_activity"])
		}
		got := map[int]time.Time{}
		for k, v := range raw {
			id, err := strconv.Atoi(k)
			if err != nil {
				t.Fatalf("last_activity key %q is not a ticket id", k)
			}
			s, ok := v.(string)
			if !ok {
				t.Fatalf("last_activity[%s] is %T, want an RFC 3339 string", k, v)
			}
			at, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				t.Fatalf("last_activity[%s] = %q: %v", k, s, err)
			}
			got[id] = at
		}
		return got
	}

	// No running tickets yet: an empty object, not null, so the UI can
	// index it without a guard.
	if got := get(t, "root", w.s.projectID); len(got) != 0 {
		t.Fatalf("fresh project: %v, want no tickets", got)
	}

	// Seed through the repository: CreateTask and SetTaskState log nothing,
	// so each ticket's activity is exactly the rows logged here.
	mk := func(t *testing.T, project, epic, story int, name string, state domain.TaskState, notes int) int {
		t.Helper()
		task, err := repo.CreateTask(ctx, domain.CreateTaskRequest{
			ProjectID: project, EpicID: epic, StoryID: story, Name: name,
			Description: "d", AcceptanceCriteria: "a", Priority: "1",
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := repo.SetTaskState(ctx, task.ID, state, ""); err != nil {
			t.Fatalf("set %s %s: %v", name, state, err)
		}
		for i := range notes {
			if err := repo.LogActivity(ctx, domain.ActivityLog{
				TaskID: task.ID, ProjectID: project, Actor: "amp-worker", Action: "comment",
				Detail: fmt.Sprintf("%s note %d", name, i),
			}); err != nil {
				t.Fatalf("log activity on %s: %v", name, err)
			}
		}
		return task.ID
	}
	newest := func(t *testing.T, task int) time.Time {
		t.Helper()
		rows, err := repo.GetTicketHistory(ctx, task)
		if err != nil {
			t.Fatalf("history of %d: %v", task, err)
		}
		if len(rows) == 0 {
			t.Fatalf("ticket %d has no activity", task)
		}
		var at time.Time
		for _, e := range rows {
			if e.CreatedAt.After(at) {
				at = e.CreatedAt
			}
		}
		return at
	}

	acme := w.s
	running := mk(t, acme.projectID, acme.epicID, acme.storyID, "running", domain.TaskStateInProgress, 3)
	quiet := mk(t, acme.projectID, acme.epicID, acme.storyID, "running, never logged", domain.TaskStateInProgress, 0)
	parked := mk(t, acme.projectID, acme.epicID, acme.storyID, "backlog", domain.TaskStateBacklog, 2)
	stuck := mk(t, acme.projectID, acme.epicID, acme.storyID, "blocked", domain.TaskStateBlocked, 2)
	done := mk(t, acme.projectID, acme.epicID, acme.storyID, "completed", domain.TaskStateCompleted, 2)

	// A running ticket with activity in a project of another org.
	sfx := time.Now().UnixNano()
	other, err := repo.CreateProject(ctx, domain.CreateProjectRequest{
		Name: fmt.Sprintf("Last activity other %d", sfx), Code: fmt.Sprintf("lao%d", sfx/1000%1000000000),
		OrgID: &w.other.ID, TeamID: &w.x.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	oe, err := repo.CreateEpic(ctx, domain.CreateEpicRequest{ProjectID: other.ID, Name: "oe", Priority: "1"})
	if err != nil {
		t.Fatal(err)
	}
	os_, err := repo.CreateStory(ctx, domain.CreateStoryRequest{ProjectID: other.ID, EpicID: oe.ID, Name: "os", Priority: "1"})
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := mk(t, other.ID, oe.ID, os_.ID, "other project, running", domain.TaskStateInProgress, 2)

	t.Run("only the project's running tickets with activity", func(t *testing.T) {
		got := get(t, "root", acme.projectID)
		if ids := slices.Sorted(maps.Keys(got)); !slices.Equal(ids, []int{running}) {
			t.Fatalf("tickets %v, want only the running one %d (quiet %d, backlog %d, blocked %d, completed %d, other project %d)",
				ids, running, quiet, parked, stuck, done, elsewhere)
		}
		if want := newest(t, running); !got[running].Equal(want) {
			t.Fatalf("running ticket's last activity %v, want its newest row's %v", got[running], want)
		}
	})

	t.Run("the other project sees only its own ticket", func(t *testing.T) {
		got := get(t, "root", other.ID)
		if ids := slices.Sorted(maps.Keys(got)); !slices.Equal(ids, []int{elsewhere}) {
			t.Fatalf("other project's tickets %v, want only %d", ids, elsewhere)
		}
		if want := newest(t, elsewhere); !got[elsewhere].Equal(want) {
			t.Fatalf("other project's last activity %v, want %v", got[elsewhere], want)
		}
	})

	t.Run("a member team's user reads it", func(t *testing.T) {
		if got := get(t, "alice", acme.projectID); len(got) != 1 {
			t.Fatalf("alice sees %v, want the one running ticket", got)
		}
	})

	t.Run("someone outside the project is refused", func(t *testing.T) {
		code, out := ra.do(t, "outsider", http.MethodGet, path(acme.projectID), nil)
		if code != http.StatusNotFound && code != http.StatusForbidden {
			t.Fatalf("outsider: %d %v, want 403 or 404", code, out)
		}
		if _, leaked := out["last_activity"]; leaked {
			t.Fatalf("outsider got last_activity: %v", out)
		}
	})

	t.Run("GET only", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			if code, _ := ra.do(t, "root", method, path(acme.projectID), nil); code != http.StatusMethodNotAllowed {
				t.Fatalf("%s: %d, want 405", method, code)
			}
		}
	})
}
