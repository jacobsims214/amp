package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Project reports ----
//
// A report counts tickets by the state a person reads (ready, in progress,
// waiting on dependencies, flagged by an agent, scheduled, done), and
// summarises a time range: what was finished, started, and created, by
// whom, and how long it took. Any metadata key can break it down, using
// each ticket's effective (inherited) value.

// StateCounts counts tickets by display state.
type StateCounts struct {
	Ready     int `json:"ready"`
	Progress  int `json:"in_progress"`
	Waiting   int `json:"waiting"`
	Flagged   int `json:"flagged"`
	Scheduled int `json:"scheduled"`
	Done      int `json:"done"`
}

func (c *StateCounts) add(t domain.Task) {
	switch {
	case t.State == domain.TaskStateCompleted:
		c.Done++
	case t.State == domain.TaskStateInProgress:
		c.Progress++
	case t.State == domain.TaskStateBlocked && strings.HasPrefix(t.BlockReason, "scheduled:"):
		c.Scheduled++
	case t.State == domain.TaskStateBlocked && t.BlockReason != "":
		c.Flagged++
	case t.State == domain.TaskStateBlocked:
		c.Waiting++
	default:
		c.Ready++
	}
}

// ReportGroup is one row of a breakdown: an agent, an epic, or a metadata value.
type ReportGroup struct {
	Key              string      `json:"key"`
	Label            string      `json:"label,omitempty"`
	Tickets          int         `json:"tickets"`
	States           StateCounts `json:"states"`
	CompletedInRange int         `json:"completed_in_range"`
	MedianHours      *float64    `json:"median_cycle_hours,omitempty"` // dispatch → done, for those finished in range
	// Usage is the token spend of runs recorded in range on this group's tickets.
	Usage  *domain.Usage `json:"usage,omitempty"`
	cycles []float64
}

func (g *ReportGroup) addUsage(runs []domain.Run) {
	if len(runs) == 0 {
		return
	}
	if g.Usage == nil {
		g.Usage = &domain.Usage{}
	}
	for _, r := range runs {
		g.Usage.Add(r)
	}
}

// MetadataKey is a key found on the project's tickets and how many carry it.
type MetadataKey struct {
	Key      string  `json:"key"`
	Coverage float64 `json:"coverage"` // share of tickets with a value
}

type ProjectReport struct {
	Since       time.Time     `json:"since"`
	Until       time.Time     `json:"until"`
	Tickets     int           `json:"tickets"`
	States      StateCounts   `json:"states"`
	Completed   int           `json:"completed"`  // in range
	Dispatched  int           `json:"dispatched"` // in range
	Created     int           `json:"created"`    // in range
	MedianHours *float64      `json:"median_cycle_hours,omitempty"`
	FlagHours   float64       `json:"flag_hours"` // how long open flags have waited, summed
	Agents      []ReportGroup `json:"agents"`
	Epics       []ReportGroup `json:"epics"`
	Keys        []MetadataKey `json:"metadata_keys"`
	GroupBy     string        `json:"group_by,omitempty"`
	Breakdown   []ReportGroup `json:"breakdown,omitempty"`
	// People groups tickets by owner (key: user id, "" for unowned): the work
	// each person and their agents did.
	People []ReportGroup `json:"people"`
	// Usage is the token spend of runs recorded in range, in total and by model.
	Usage        domain.Usage `json:"usage"`
	UsageByModel []UsageRow   `json:"usage_by_model"`
}

func median(xs []float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return &m
}

// ProjectReport builds a project's report for [since, until), broken down
// by groupBy (a metadata key) when it is set.
func (s *Service) ProjectReport(ctx context.Context, p authz.Principal, projectID int, since, until time.Time, groupBy string) (*ProjectReport, error) {
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
		return nil, err
	}
	if !until.After(since) {
		return nil, authz.BadRequest("until must be after since")
	}
	tasks, err := s.Repo.ProjectTasksWithMeta(ctx, projectID)
	if err != nil {
		return nil, err
	}
	epics, err := s.Repo.ListEpics(ctx, projectID)
	if err != nil {
		return nil, err
	}
	runs, err := s.Repo.ProjectRuns(ctx, projectID, since, until)
	if err != nil {
		return nil, err
	}
	runsByTask := map[int][]domain.Run{}
	for _, run := range runs {
		runsByTask[run.TaskID] = append(runsByTask[run.TaskID], run)
	}
	in := func(t *time.Time) bool { return t != nil && !t.Before(since) && t.Before(until) }
	now := time.Now()

	r := &ProjectReport{Since: since, Until: until, Tickets: len(tasks), Agents: []ReportGroup{}, Epics: []ReportGroup{}, Keys: []MetadataKey{}, GroupBy: groupBy,
		People: []ReportGroup{}, UsageByModel: usageByModel(runs)}
	for _, run := range runs {
		r.Usage.Add(run)
	}
	people := map[string]*ReportGroup{}
	agents := map[string]*ReportGroup{}
	epicRows := map[int]*ReportGroup{}
	for _, e := range epics {
		epicRows[e.ID] = &ReportGroup{Key: fmt.Sprint(e.ID), Label: e.Name}
	}
	groups := map[string]*ReportGroup{}
	keyCount := map[string]int{}
	var cycles []float64

	count := func(g *ReportGroup, t domain.Task, cycle float64, done bool) {
		g.addUsage(runsByTask[t.ID])
		g.Tickets++
		g.States.add(t)
		if done {
			g.CompletedInRange++
			if cycle >= 0 {
				g.cycles = append(g.cycles, cycle)
			}
		}
	}
	for _, x := range tasks {
		t := x.Task
		r.States.add(t)
		doneInRange := t.State == domain.TaskStateCompleted && in(t.CompletedAt)
		cycle := -1.0
		if doneInRange {
			r.Completed++
			if t.DispatchedAt != nil {
				cycle = t.CompletedAt.Sub(*t.DispatchedAt).Hours()
				cycles = append(cycles, cycle)
			}
		}
		if in(t.DispatchedAt) {
			r.Dispatched++
		}
		created := t.CreatedAt
		if in(&created) {
			r.Created++
		}
		if t.State == domain.TaskStateBlocked && t.BlockReason != "" && !strings.HasPrefix(t.BlockReason, "scheduled:") {
			r.FlagHours += now.Sub(t.UpdatedAt).Hours()
		}
		if t.AgentID != "" {
			g := agents[t.AgentID]
			if g == nil {
				g = &ReportGroup{Key: t.AgentID}
				agents[t.AgentID] = g
			}
			count(g, t, cycle, doneInRange)
		}
		if g := epicRows[t.EpicID]; g != nil {
			count(g, t, cycle, doneInRange)
		}
		owner := ""
		if t.OwnerUserID != nil {
			owner = fmt.Sprint(*t.OwnerUserID)
		}
		if people[owner] == nil {
			people[owner] = &ReportGroup{Key: owner}
		}
		count(people[owner], t, cycle, doneInRange)
		for k := range x.Effective {
			keyCount[k]++
		}
		if groupBy != "" {
			v := "(not set)"
			if val, ok := x.Effective[groupBy]; ok && val != nil {
				v = fmt.Sprint(val)
			}
			g := groups[v]
			if g == nil {
				g = &ReportGroup{Key: v}
				groups[v] = g
			}
			count(g, t, cycle, doneInRange)
		}
	}
	r.MedianHours = median(cycles)
	finish := func(m map[string]*ReportGroup) []ReportGroup {
		out := make([]ReportGroup, 0, len(m))
		for _, g := range m {
			g.MedianHours = median(g.cycles)
			out = append(out, *g)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].CompletedInRange != out[j].CompletedInRange {
				return out[i].CompletedInRange > out[j].CompletedInRange
			}
			if out[i].Tickets != out[j].Tickets {
				return out[i].Tickets > out[j].Tickets
			}
			return out[i].Key < out[j].Key
		})
		return out
	}
	r.Agents = finish(agents)
	r.People = finish(people)
	if groupBy != "" {
		r.Breakdown = finish(groups)
	}
	for _, e := range epics {
		g := epicRows[e.ID]
		g.MedianHours = median(g.cycles)
		r.Epics = append(r.Epics, *g)
	}
	for k, n := range keyCount {
		r.Keys = append(r.Keys, MetadataKey{Key: k, Coverage: float64(n) / float64(max(1, len(tasks)))})
	}
	sort.Slice(r.Keys, func(i, j int) bool {
		if r.Keys[i].Coverage != r.Keys[j].Coverage {
			return r.Keys[i].Coverage > r.Keys[j].Coverage
		}
		return r.Keys[i].Key < r.Keys[j].Key
	})
	return r, nil
}
