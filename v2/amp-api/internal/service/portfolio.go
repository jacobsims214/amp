package service

import (
	"context"
	"sort"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Portfolio: an org's projects side by side ----
//
// Nothing here assumes a particular tracker or finance scheme. Projects carry
// whatever metadata the org uses (jira_board, linear_team, ado_area_path,
// cost_center, …) and the viewer picks a key to split spend by
// (capitalizable, cost_type, …), using effective values (project → epic →
// story → ticket).

// SplitGroup is the spend and tickets for one value of the split key.
type SplitGroup struct {
	Tickets int          `json:"tickets"`
	Usage   domain.Usage `json:"usage"`
}

// PortfolioRow is one project's progress and token spend.
type PortfolioRow struct {
	ProjectID int             `json:"project_id"`
	Name      string          `json:"name"`
	Code      string          `json:"code"`
	State     string          `json:"state"`
	Metadata  domain.Metadata `json:"metadata,omitempty"` // the project's own
	Tickets   int             `json:"tickets"`
	States    StateCounts     `json:"states"`
	Completed int             `json:"completed"` // in range
	Usage     domain.Usage    `json:"usage"`
	// Split is spend and tickets by the effective value of the split key;
	// "(not set)" collects tickets without one.
	Split map[string]*SplitGroup `json:"split"`
}

// Portfolio is an org's projects (the ones the viewer can see) over a range.
type Portfolio struct {
	OrgID   int            `json:"org_id"`
	Since   time.Time      `json:"since"`
	Until   time.Time      `json:"until"`
	SplitBy string         `json:"split_by,omitempty"`
	// SplitValues are the split key's values seen, most spend first.
	SplitValues []string `json:"split_values"`
	// ProjectKeys are metadata keys set on the projects (for columns);
	// TicketKeys are keys on their tickets' effective metadata (for splitting).
	ProjectKeys []string       `json:"project_keys"`
	TicketKeys  []MetadataKey  `json:"ticket_keys"`
	Projects    []PortfolioRow `json:"projects"`
	Total       PortfolioRow   `json:"total"`
	ByModel     []UsageRow     `json:"usage_by_model"`
}

func addUsage(dst *domain.Usage, src *domain.Usage) {
	if src == nil {
		return
	}
	dst.Runs += src.Runs
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheWriteTokens += src.CacheWriteTokens
	dst.CostUSD += src.CostUSD
	dst.Unreported += src.Unreported
	dst.Abandoned += src.Abandoned
}

func addSplit(m map[string]*SplitGroup, key string, tickets int, u *domain.Usage) {
	g := m[key]
	if g == nil {
		g = &SplitGroup{}
		m[key] = g
	}
	g.Tickets += tickets
	addUsage(&g.Usage, u)
}

// OrgPortfolio builds the portfolio for orgID: every project in it that p can
// see, with progress and spend in [since, until), split by splitBy (any
// metadata key; empty for no split).
func (s *Service) OrgPortfolio(ctx context.Context, p authz.Principal, orgID int, since, until time.Time, splitBy string) (*Portfolio, error) {
	if _, err := s.orgAccess(ctx, p, orgID); err != nil {
		return nil, err
	}
	visible, err := s.Authz.VisibleProjects(ctx, p)
	if err != nil {
		return nil, err
	}
	projects, err := s.Repo.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := &Portfolio{OrgID: orgID, Since: since, Until: until, SplitBy: splitBy, SplitValues: []string{},
		ProjectKeys: []string{}, TicketKeys: []MetadataKey{}, Projects: []PortfolioRow{}, ByModel: []UsageRow{}}
	out.Total.Split = map[string]*SplitGroup{}
	projectKeys := map[string]bool{}
	ticketKeys := map[string]float64{} // key → tickets carrying it
	var allRuns []domain.Run
	for _, proj := range projects {
		if proj.OrgID == nil || *proj.OrgID != orgID || (visible != nil && !visible[proj.ID]) {
			continue
		}
		rep, err := s.ProjectReport(ctx, p, proj.ID, since, until, splitBy)
		if err != nil {
			return nil, err
		}
		row := PortfolioRow{ProjectID: proj.ID, Name: proj.Name, Code: proj.Code, State: string(proj.State), Metadata: proj.Metadata,
			Tickets: rep.Tickets, States: rep.States, Completed: rep.Completed, Usage: rep.Usage, Split: map[string]*SplitGroup{}}
		for _, g := range rep.Breakdown {
			addSplit(row.Split, g.Key, g.Tickets, g.Usage)
			addSplit(out.Total.Split, g.Key, g.Tickets, g.Usage)
		}
		for k := range proj.Metadata {
			projectKeys[k] = true
		}
		for _, k := range rep.Keys {
			ticketKeys[k.Key] += k.Coverage * float64(rep.Tickets)
		}
		out.Projects = append(out.Projects, row)
		t := &out.Total
		t.Tickets += row.Tickets
		t.Completed += row.Completed
		addUsage(&t.Usage, &row.Usage)
		st := &t.States
		st.Ready += row.States.Ready
		st.Progress += row.States.Progress
		st.Waiting += row.States.Waiting
		st.Flagged += row.States.Flagged
		st.Scheduled += row.States.Scheduled
		st.Done += row.States.Done
		runs, err := s.Repo.ProjectRuns(ctx, proj.ID, since, until)
		if err != nil {
			return nil, err
		}
		allRuns = append(allRuns, runs...)
	}
	out.Total.Name = "All projects"
	out.ByModel = usageByModel(allRuns)
	for k := range projectKeys {
		out.ProjectKeys = append(out.ProjectKeys, k)
	}
	sort.Strings(out.ProjectKeys)
	for k, n := range ticketKeys {
		out.TicketKeys = append(out.TicketKeys, MetadataKey{Key: k, Coverage: n / float64(max(1, out.Total.Tickets))})
	}
	sort.Slice(out.TicketKeys, func(i, j int) bool { return out.TicketKeys[i].Coverage > out.TicketKeys[j].Coverage })
	for v := range out.Total.Split {
		out.SplitValues = append(out.SplitValues, v)
	}
	sort.Slice(out.SplitValues, func(i, j int) bool {
		a, b := out.Total.Split[out.SplitValues[i]], out.Total.Split[out.SplitValues[j]]
		if a.Usage.CostUSD != b.Usage.CostUSD {
			return a.Usage.CostUSD > b.Usage.CostUSD
		}
		return out.SplitValues[i] < out.SplitValues[j]
	})
	sort.Slice(out.Projects, func(i, j int) bool {
		return out.Projects[i].Usage.CostUSD > out.Projects[j].Usage.CostUSD ||
			(out.Projects[i].Usage.CostUSD == out.Projects[j].Usage.CostUSD && out.Projects[i].Name < out.Projects[j].Name)
	})
	return out, nil
}
