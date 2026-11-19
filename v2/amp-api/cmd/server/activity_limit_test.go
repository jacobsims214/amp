package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/simstech/amp-api/internal/domain"
)

// GET /api/projects/:id/activity?limit=N returns the newest N rows of the
// project's activity log (AMP #1579). With no limit, or one that isn't a
// usable number, it returns every row, exactly as before the param existed.
// The response shape stays {"activity": [...], "count": n}.
func TestRest_ProjectActivityLimit(t *testing.T) {
	w := newRBACWorld(t)
	ra := newRestAs(t, w)
	ctx := context.Background()
	root := w.s.as(w.root)

	task := mkIn(t, root, w.s.storyID, "activity limit")
	const seeded = 12
	for i := range seeded {
		if err := w.s.repo.LogActivity(ctx, domain.ActivityLog{
			TaskID: task, ProjectID: w.s.projectID, Actor: "amp-worker", Action: "comment",
			Detail: fmt.Sprintf("note %d", i),
		}); err != nil {
			t.Fatalf("seed activity: %v", err)
		}
	}

	path := fmt.Sprintf("/api/projects/%d/activity", w.s.projectID)
	get := func(t *testing.T, query string) []int {
		t.Helper()
		out := ra.want(t, 200, "root", http.MethodGet, path+query, nil)
		if len(out) != 2 {
			t.Fatalf("GET %s%s: keys %v, want just activity and count", path, query, out)
		}
		rows, ok := out["activity"].([]any)
		if !ok {
			t.Fatalf("GET %s%s: activity is %T, want a list", path, query, out["activity"])
		}
		if out["count"] != float64(len(rows)) {
			t.Fatalf("GET %s%s: count %v for %d rows", path, query, out["count"], len(rows))
		}
		ids := make([]int, len(rows))
		for i, row := range rows {
			ids[i] = int(row.(map[string]any)["id"].(float64))
		}
		return ids
	}

	// No limit: the same rows, in the same order, as the unbounded query the
	// handler ran before #1579.
	before, err := w.s.repo.GetProjectActivity(ctx, w.s.projectID, "", "", 0)
	if err != nil {
		t.Fatalf("unbounded repo read: %v", err)
	}
	all := make([]int, len(before))
	for i, e := range before {
		all[i] = e.ID
	}
	if len(all) < seeded {
		t.Fatalf("project has %d activity rows, want at least the %d seeded", len(all), seeded)
	}
	if got := get(t, ""); !slices.Equal(got, all) {
		t.Fatalf("no limit: ids %v, want %v", got, all)
	}

	for _, tc := range []struct {
		name, query string
		want        []int
	}{
		{"limit 5 is the newest 5", "?limit=5", all[:5]},
		{"limit 1", "?limit=1", all[:1]},
		{"limit above the row count", "?limit=400", all},
		{"limit 0 is unbounded", "?limit=0", all},
		{"negative limit is unbounded", "?limit=-3", all},
		{"empty limit is unbounded", "?limit=", all},
		{"non-numeric limit is unbounded", "?limit=five", all},
		{"overflowing limit is unbounded", "?limit=99999999999999999999", all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := get(t, tc.query); !slices.Equal(got, tc.want) {
				t.Fatalf("GET %s: ids %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}
