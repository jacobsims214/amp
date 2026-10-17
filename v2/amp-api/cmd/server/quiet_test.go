package main

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A ticket in progress whose worker went silent is listed as quiet, so a
// manager starting a new session can find work orphaned by a dead one.
func TestMCP_ListTasksReportsQuiet(t *testing.T) {
	s := setupMCP(t)
	live := mkIn(t, s, s.storyID, "worker still talking")
	dead := mkIn(t, s, s.storyID, "worker died")
	for _, id := range []int{live, dead} {
		s.call(t, "amp_dispatch_task", map[string]interface{}{"task_id": float64(id), "agent_id": "amp-worker"})
	}

	// The dead ticket's last sign of life was 40 minutes ago.
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`UPDATE activity_log SET created_at = NOW() - INTERVAL '40 minutes' WHERE task_id = $1`,
		`UPDATE tasks SET dispatched_at = NOW() - INTERVAL '40 minutes' WHERE id = $1`,
	} {
		if _, err := conn.Exec(ctx, q, dead); err != nil {
			t.Fatal(err)
		}
	}

	out := s.call(t, "amp_list_tasks", map[string]interface{}{"project_id": float64(s.projectID), "owner": "any"})
	quiet := map[int]float64{}
	for _, q := range out["quiet"].([]interface{}) {
		m := q.(map[string]interface{})
		quiet[int(m["id"].(float64))] = m["silent_minutes"].(float64)
	}
	if mins, ok := quiet[dead]; !ok || mins < 39 {
		t.Fatalf("dead worker's ticket not quiet: %v", out["quiet"])
	}
	if _, ok := quiet[live]; ok {
		t.Fatalf("a ticket with fresh activity is reported quiet: %v", out["quiet"])
	}
}
