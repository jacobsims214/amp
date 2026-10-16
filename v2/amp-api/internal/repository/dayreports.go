package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/simstech/amp-api/internal/domain"
)

// BuildDayReportData gathers a user's day, between start and end:
// tickets they own that completed, tickets they started (dispatched) or
// created, other tickets they worked on, and — as of now — their tickets
// in progress and blocked.
func (r *Repo) BuildDayReportData(ctx context.Context, userID int, start, end time.Time) (domain.DayReportData, error) {
	ids := func(q string) ([]int, error) {
		rows, err := r.db.Query(ctx, q, userID, start, end)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[int])
	}
	completed, err := ids(`SELECT id FROM tasks WHERE owner_user_id = $1 AND completed_at >= $2 AND completed_at < $3 ORDER BY completed_at`)
	if err != nil {
		return domain.DayReportData{}, err
	}
	started, err := ids(`SELECT task_id FROM activity_log WHERE actor_user_id = $1 AND action = 'dispatched'
		AND created_at >= $2 AND created_at < $3 GROUP BY task_id ORDER BY MIN(created_at)`)
	if err != nil {
		return domain.DayReportData{}, err
	}
	created, err := ids(`SELECT id FROM tasks WHERE created_by = $1 AND created_at >= $2 AND created_at < $3 ORDER BY created_at`)
	if err != nil {
		return domain.DayReportData{}, err
	}
	touched, err := ids(`SELECT task_id FROM activity_log WHERE actor_user_id = $1 AND created_at >= $2 AND created_at < $3
		GROUP BY task_id ORDER BY MIN(created_at)`)
	if err != nil {
		return domain.DayReportData{}, err
	}
	// In progress and blocked: their tickets as they stand now.
	owned := func(state domain.TaskState) ([]int, error) {
		rows, err := r.db.Query(ctx, `SELECT id FROM tasks WHERE owner_user_id = $1 AND state = $2 ORDER BY updated_at`, userID, state)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[int])
	}
	inProgress, err := owned(domain.TaskStateInProgress)
	if err != nil {
		return domain.DayReportData{}, err
	}
	blocked, err := owned(domain.TaskStateBlocked)
	if err != nil {
		return domain.DayReportData{}, err
	}

	all := map[int]bool{}
	for _, l := range [][]int{completed, started, created, touched, inProgress, blocked} {
		for _, id := range l {
			all[id] = true
		}
	}
	items, err := r.dayReportItems(ctx, all)
	if err != nil {
		return domain.DayReportData{}, err
	}
	pick := func(l []int, skip ...map[int]bool) []domain.DayReportItem {
		out := make([]domain.DayReportItem, 0, len(l))
	next:
		for _, id := range l {
			for _, s := range skip {
				if s[id] {
					continue next
				}
			}
			if it, ok := items[id]; ok {
				out = append(out, it)
			}
		}
		return out
	}
	set := func(l []int) map[int]bool {
		m := map[int]bool{}
		for _, id := range l {
			m[id] = true
		}
		return m
	}
	// Each ticket appears once: completed, in progress, and blocked first;
	// started and created only for tickets in none of those; anything else
	// they touched that day last.
	done, running, stuck := set(completed), set(inProgress), set(blocked)
	d := domain.DayReportData{
		Completed:  pick(completed),
		InProgress: pick(inProgress),
		Blocked:    pick(blocked),
		Started:    pick(started, done, running, stuck),
		Created:    pick(created, done, running, stuck, set(started)),
	}
	d.Touched = pick(touched, done, running, stuck, set(started), set(created))
	d.Stats = d.ComputeStats()
	d.GeneratedAt = time.Now()
	return d, nil
}

func (r *Repo) dayReportItems(ctx context.Context, ids map[int]bool) (map[int]domain.DayReportItem, error) {
	out := map[int]domain.DayReportItem{}
	if len(ids) == 0 {
		return out, nil
	}
	list := make([]int, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	rows, err := r.db.Query(ctx, `
		SELECT t.id, t.name, t.state, t.project_id, p.code, p.name, t.epic_id, e.name, t.story_id, s.name,
		       t.block_reason, t.completed_at, t.created_at, p.metadata || e.metadata || s.metadata || t.metadata
		FROM tasks t
		JOIN projects p ON p.id = t.project_id
		JOIN epics e ON e.id = t.epic_id
		JOIN stories s ON s.id = t.story_id
		WHERE t.id = ANY($1)`, list)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it domain.DayReportItem
		var createdAt time.Time
		if err := rows.Scan(&it.TaskID, &it.Name, &it.State, &it.ProjectID, &it.ProjectCode, &it.ProjectName,
			&it.EpicID, &it.EpicName, &it.StoryID, &it.StoryName, &it.BlockReason, &it.CompletedAt, &createdAt, &it.Metadata); err != nil {
			return nil, err
		}
		it.Ref = domain.TaskRef(it.ProjectCode, it.TaskID)
		if it.CompletedAt != nil {
			h := it.CompletedAt.Sub(createdAt).Hours()
			it.CycleTimeHours = &h
		}
		if it.State != string(domain.TaskStateBlocked) {
			it.BlockReason = ""
		}
		out[it.ID()] = it
	}
	return out, rows.Err()
}

const dayReportCols = `r.user_id, u.email, u.display_name, to_char(r.day, 'YYYY-MM-DD'), r.tz, r.summary, r.data, r.created_at, r.updated_at`

func scanDayReport(row rowScanner, d *domain.DayReport) error {
	var raw []byte
	var created, updated time.Time
	if err := row.Scan(&d.UserID, &d.UserEmail, &d.UserName, &d.Day, &d.TZ, &d.Summary, &raw, &created, &updated); err != nil {
		return err
	}
	d.Saved, d.CreatedAt, d.UpdatedAt = true, &created, &updated
	return json.Unmarshal(raw, &d.Data)
}

// GetDayReport returns a user's saved report for a day, or nil.
func (r *Repo) GetDayReport(ctx context.Context, userID int, day string) (*domain.DayReport, error) {
	d := &domain.DayReport{}
	err := scanDayReport(r.db.QueryRow(ctx, `SELECT `+dayReportCols+` FROM day_reports r JOIN users u ON u.id = r.user_id
		WHERE r.user_id = $1 AND r.day = $2::date`, userID, day), d)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return d, err
}

// SaveDayReport creates or replaces a user's report for a day.
func (r *Repo) SaveDayReport(ctx context.Context, userID int, day, tz, summary string, data domain.DayReportData) (*domain.DayReport, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if _, err := r.db.Exec(ctx, `
		INSERT INTO day_reports (user_id, day, tz, summary, data) VALUES ($1, $2::date, $3, $4, $5)
		ON CONFLICT (user_id, day) DO UPDATE SET tz = EXCLUDED.tz, summary = EXCLUDED.summary,
		  data = EXCLUDED.data, updated_at = NOW()`, userID, day, tz, summary, raw); err != nil {
		return nil, err
	}
	return r.GetDayReport(ctx, userID, day)
}

// ListDayReports returns a user's saved reports from one day to another
// (inclusive), newest first.
func (r *Repo) ListDayReports(ctx context.Context, userID int, from, to string) ([]domain.DayReport, error) {
	rows, err := r.db.Query(ctx, `SELECT `+dayReportCols+` FROM day_reports r JOIN users u ON u.id = r.user_id
		WHERE r.user_id = $1 AND r.day BETWEEN $2::date AND $3::date ORDER BY r.day DESC`, userID, from, to)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanDayReport)
}

// VelocityRow is one project's completed tickets on one day.
type VelocityRow struct {
	Day         string
	ProjectID   int
	ProjectCode string
	Completed   int
}

// Velocity counts the tickets a user owns that completed each day between
// start and end, by project, with days in tz.
func (r *Repo) Velocity(ctx context.Context, userID int, start, end time.Time, tz string) ([]VelocityRow, error) {
	rows, err := r.db.Query(ctx, `
		SELECT to_char((t.completed_at AT TIME ZONE $4)::date, 'YYYY-MM-DD'), t.project_id, p.code, COUNT(*)
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE t.owner_user_id = $1 AND t.completed_at >= $2 AND t.completed_at < $3
		GROUP BY 1, 2, 3 ORDER BY 1, 3`, userID, start, end, tz)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row rowScanner, v *VelocityRow) error {
		return row.Scan(&v.Day, &v.ProjectID, &v.ProjectCode, &v.Completed)
	})
}

// SharesOrg reports whether two users are members of at least one common org.
func (r *Repo) SharesOrg(ctx context.Context, a, b int) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_members x JOIN org_members y ON y.org_id = x.org_id
		WHERE x.user_id = $1 AND y.user_id = $2)`, a, b).Scan(&ok)
	return ok, err
}

// ReportPeople lists the users whose reports viewerID can read: everyone
// sharing an org with them, and themselves (all users when viewerID is 0).
func (r *Repo) ReportPeople(ctx context.Context, viewerID int) ([]domain.Member, error) {
	q := `SELECT u.id, u.email, u.display_name, '' FROM users u ORDER BY u.display_name, u.email`
	args := []any{}
	if viewerID != 0 {
		q = `SELECT DISTINCT u.id, u.email, u.display_name, '' FROM users u
		     WHERE u.id = $1 OR u.id IN (SELECT y.user_id FROM org_members x JOIN org_members y ON y.org_id = x.org_id WHERE x.user_id = $1)
		     ORDER BY u.display_name, u.email`
		args = append(args, viewerID)
	}
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("report people: %w", err)
	}
	return collect(rows, scanMember)
}
