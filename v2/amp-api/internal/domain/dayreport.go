package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// DayReport is one person's end-of-day report: what they got done, what's
// in flight and blocked, and a summary in their own words. Data is a
// snapshot of their tickets taken when the report is generated or saved.
type DayReport struct {
	UserID      int           `json:"user_id"`
	UserEmail   string        `json:"user_email"`
	UserName    string        `json:"user_name"`
	Day         string        `json:"day"` // YYYY-MM-DD in TZ
	TZ          string        `json:"tz"`
	Summary     string        `json:"summary"`
	Data        DayReportData `json:"data"`
	Saved       bool          `json:"saved"`
	CreatedAt   *time.Time    `json:"created_at,omitempty"`
	UpdatedAt   *time.Time    `json:"updated_at,omitempty"`
	Markdown    string        `json:"markdown"`
	HiddenItems int           `json:"hidden_items,omitempty"` // tickets in projects the viewer can't see
}

// DayReportData is the structured part of a report, for integrations.
type DayReportData struct {
	Completed  []DayReportItem `json:"completed"`   // completed that day
	Started    []DayReportItem `json:"started"`     // dispatched that day
	Created    []DayReportItem `json:"created"`     // created that day
	InProgress []DayReportItem `json:"in_progress"` // in progress when the report was made
	Blocked    []DayReportItem `json:"blocked"`     // blocked when the report was made
	Touched    []DayReportItem `json:"touched"`     // other tickets worked on that day (comments, state changes, …)
	Stats      DayReportStats  `json:"stats"`
	// GeneratedAt is when the snapshot was taken.
	GeneratedAt time.Time `json:"generated_at"`
}

// DayReportStats summarises the day.
type DayReportStats struct {
	Completed     int      `json:"completed"`
	Started       int      `json:"started"`
	Created       int      `json:"created"`
	InProgress    int      `json:"in_progress"`
	Blocked       int      `json:"blocked"`
	Projects      int      `json:"projects"`
	AvgCycleHours *float64 `json:"avg_cycle_hours,omitempty"` // created → completed, for completed tickets
}

// DayReportItem is a ticket in a report, with where it lives and its
// effective metadata (e.g. a jira_key), so integrations can map it.
type DayReportItem struct {
	TaskID         int        `json:"task_id"`
	Ref            string     `json:"ref"` // e.g. STORE-12
	Name           string     `json:"name"`
	State          string     `json:"state"`
	ProjectID      int        `json:"project_id"`
	ProjectCode    string     `json:"project_code"`
	ProjectName    string     `json:"project_name"`
	EpicID         int        `json:"epic_id"`
	EpicName       string     `json:"epic_name"`
	StoryID        int        `json:"story_id"`
	StoryName      string     `json:"story_name"`
	BlockReason    string     `json:"block_reason,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	CycleTimeHours *float64   `json:"cycle_time_hours,omitempty"`
	Metadata       Metadata   `json:"metadata"`
}

// ID is the item's task id.
func (it DayReportItem) ID() int { return it.TaskID }

// TaskRef is how a ticket is referred to in reports: project code and id.
func TaskRef(projectCode string, taskID int) string {
	return fmt.Sprintf("%s-%d", strings.ToUpper(projectCode), taskID)
}

// Filter keeps only items in the given projects (nil keeps everything) and
// recomputes the stats; it returns how many items were dropped.
func (d DayReportData) Filter(projects map[int]bool) (DayReportData, int) {
	if projects == nil {
		return d, 0
	}
	hidden := 0
	keep := func(items []DayReportItem) []DayReportItem {
		out := make([]DayReportItem, 0, len(items))
		for _, it := range items {
			if projects[it.ProjectID] {
				out = append(out, it)
			} else {
				hidden++
			}
		}
		return out
	}
	d.Completed, d.Started, d.Created = keep(d.Completed), keep(d.Started), keep(d.Created)
	d.InProgress, d.Blocked, d.Touched = keep(d.InProgress), keep(d.Blocked), keep(d.Touched)
	d.Stats = d.ComputeStats()
	return d, hidden
}

// ComputeStats derives the stats from the item lists.
func (d DayReportData) ComputeStats() DayReportStats {
	s := DayReportStats{
		Completed: len(d.Completed), Started: len(d.Started), Created: len(d.Created),
		InProgress: len(d.InProgress), Blocked: len(d.Blocked),
	}
	projects := map[int]bool{}
	var hours []float64
	for _, list := range [][]DayReportItem{d.Completed, d.Started, d.Created, d.InProgress, d.Blocked, d.Touched} {
		for _, it := range list {
			projects[it.ProjectID] = true
		}
	}
	for _, it := range d.Completed {
		if it.CycleTimeHours != nil {
			hours = append(hours, *it.CycleTimeHours)
		}
	}
	s.Projects = len(projects)
	if len(hours) > 0 {
		sum := 0.0
		for _, h := range hours {
			sum += h
		}
		avg := sum / float64(len(hours))
		s.AvgCycleHours = &avg
	}
	return s
}

// RenderMarkdown is the report as markdown, ready to paste into chat or a
// status update.
func (r DayReport) RenderMarkdown() string {
	var b strings.Builder
	who := r.UserName
	if who == "" {
		who = r.UserEmail
	}
	day := r.Day
	if t, err := time.Parse("2006-01-02", r.Day); err == nil {
		day = t.Format("Mon, Jan 2 2006")
	}
	fmt.Fprintf(&b, "# Daily report: %s · %s\n\n", who, day)
	if s := strings.TrimSpace(r.Summary); s != "" {
		b.WriteString(s + "\n\n")
	}
	st := r.Data.Stats
	parts := []string{fmt.Sprintf("%d completed", st.Completed), fmt.Sprintf("%d in progress", st.InProgress)}
	if st.Blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", st.Blocked))
	}
	fmt.Fprintf(&b, "**%s** across %d project%s\n", strings.Join(parts, " · "), st.Projects, plural(st.Projects))

	section := func(title string, items []DayReportItem, extra func(DayReportItem) string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s (%d)\n", title, len(items))
		for _, g := range groupByProject(items) {
			fmt.Fprintf(&b, "\n**%s**\n", g.name)
			for _, it := range g.items {
				line := fmt.Sprintf("- %s %s", it.Ref, it.Name)
				if where := strings.Trim(it.EpicName+" › "+it.StoryName, " ›"); where != "" {
					line += " — " + where
				}
				if extra != nil {
					if x := extra(it); x != "" {
						line += " " + x
					}
				}
				b.WriteString(line + "\n")
			}
		}
	}
	section("Completed", r.Data.Completed, nil)
	section("In progress", r.Data.InProgress, nil)
	section("Blocked", r.Data.Blocked, func(it DayReportItem) string {
		if it.BlockReason == "" {
			return ""
		}
		return "(blocked: " + it.BlockReason + ")"
	})
	section("Started", r.Data.Started, nil)
	section("Created", r.Data.Created, nil)
	section("Also worked on", r.Data.Touched, nil)
	if r.HiddenItems > 0 {
		fmt.Fprintf(&b, "\n_%d more ticket%s in projects you can't see._\n", r.HiddenItems, plural(r.HiddenItems))
	}
	return b.String()
}

type projectGroup struct {
	name  string
	items []DayReportItem
}

func groupByProject(items []DayReportItem) []projectGroup {
	idx := map[int]int{}
	var out []projectGroup
	for _, it := range items {
		i, ok := idx[it.ProjectID]
		if !ok {
			i = len(out)
			idx[it.ProjectID] = i
			out = append(out, projectGroup{name: it.ProjectName})
		}
		out[i].items = append(out[i].items, it)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].name < out[b].name })
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// VelocityDay is one day of completed tickets.
type VelocityDay struct {
	Day       string         `json:"day"`
	Completed int            `json:"completed"`
	ByProject map[string]int `json:"by_project"` // project code → completed
}
