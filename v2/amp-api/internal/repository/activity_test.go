package repository

// Tests for GetProjectActivity's limit (AMP #1573). TestProjectActivityLimit is
// pure; TestGetProjectActivityLimit runs against a real Postgres only when
// TEST_DATABASE_URL is set, like kb_test.go:
//
//	TEST_DATABASE_URL=postgres://amp:amp@localhost:5432/amp_test?sslmode=disable \
//	go test ./internal/repository/... -run ProjectActivity -count=1

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

func TestProjectActivityLimit(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{"negative is unbounded", -1, 0},
		{"zero is unbounded", 0, 0},
		{"one", 1, 1},
		{"under the cap", 14, 14},
		{"at the cap", 500, 500},
		{"over the cap is clamped", 501, 500},
		{"far over the cap is clamped", 1_000_000, 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := projectActivityLimit(tt.limit); got != tt.want {
				t.Errorf("projectActivityLimit(%d) = %d, want %d", tt.limit, got, tt.want)
			}
		})
	}
}

func TestGetProjectActivityLimit(t *testing.T) {
	r := kbTestRepo(t)
	ctx := context.Background()

	// One project with more rows than the cap, each a second older than the
	// last, so "newest" is unambiguous.
	const total = maxProjectActivity + 10
	var project, epic, story, task int
	code := fmt.Sprintf("act%06d", rand.IntN(1_000_000))
	mustScan := func(dst *int, q string, args ...any) {
		t.Helper()
		if err := r.db.QueryRow(ctx, q, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustScan(&project, `INSERT INTO projects (name, code) VALUES ($1, $2) RETURNING id`, "activity "+code, code)
	mustScan(&epic, `INSERT INTO epics (project_id, name) VALUES ($1, 'epic') RETURNING id`, project)
	mustScan(&story, `INSERT INTO stories (project_id, epic_id, name) VALUES ($1, $2, 'story') RETURNING id`, project, epic)
	mustScan(&task, `INSERT INTO tasks (project_id, epic_id, story_id, name) VALUES ($1, $2, $3, 'task') RETURNING id`,
		project, epic, story)
	if _, err := r.db.Exec(ctx,
		`INSERT INTO activity_log (task_id, project_id, action, detail, created_at)
		 SELECT $1, $2, 'comment', g::text, now() - make_interval(secs => g)
		 FROM generate_series(1, $3::int) g`, task, project, total); err != nil {
		t.Fatalf("seed activity: %v", err)
	}

	all, err := r.GetProjectActivity(ctx, project, "", "", 0)
	if err != nil {
		t.Fatalf("unbounded: %v", err)
	}
	if len(all) != total {
		t.Fatalf("unbounded returned %d rows, want %d", len(all), total)
	}
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.After(all[i-1].CreatedAt) {
			t.Fatalf("unbounded rows not newest first at %d: %v after %v", i, all[i].CreatedAt, all[i-1].CreatedAt)
		}
	}

	sameRows := func(t *testing.T, got, want []domain.ActivityLog) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i].ID {
				t.Fatalf("row %d: id %d, want %d", i, got[i].ID, want[i].ID)
			}
		}
	}

	// since keeps the 10 newest rows (detail 1..10); a limit then applies on
	// top of it, which also checks the LIMIT placeholder follows since's.
	since := all[9].CreatedAt.Format(time.RFC3339Nano)
	tests := []struct {
		name  string
		since string
		limit int
		want  []domain.ActivityLog
	}{
		{"negative limit is unbounded", "", -1, all},
		{"limit under the cap returns the newest rows", "", 14, all[:14]},
		{"limit at the cap", "", maxProjectActivity, all[:maxProjectActivity]},
		{"limit over the cap is clamped to 500", "", 10_000, all[:maxProjectActivity]},
		{"since without a limit", since, 0, all[:10]},
		{"since with a limit", since, 4, all[:4]},
		{"since with a limit above its rows", since, 50, all[:10]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.GetProjectActivity(ctx, project, tt.since, "", tt.limit)
			if err != nil {
				t.Fatalf("GetProjectActivity: %v", err)
			}
			sameRows(t, got, tt.want)
		})
	}
}
