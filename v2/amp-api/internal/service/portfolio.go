package service

import (
	"context"
	"sort"
	"time"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Portfolio: an org's projects side by side ----

// PortfolioRow is one project's progress and token spend, split by the
// capitalizable metadata (effective values: project → epic → story → ticket).
type PortfolioRow struct {
	ProjectID  int             `json:"project_id"`
	Name       string          `json:"name"`
	Code       string          `json:"code"`
	State      string          `json:"state"`
	Metadata   domain.Metadata `json:"metadata,omitempty"` // the project's own (jira_board, cost_center, …)
	Tickets    int             `json:"tickets"`
	States     StateCounts     `json:"states"`
	Completed  int             `json:"completed"` // in range
	Usage      domain.Usage    `json:"usage"`
	Capex      domain.Usage    `json:"capex"`        // capitalizable = true
	Opex       domain.Usage    `json:"opex"`         // capitalizable = false
	Unassigned domain.Usage    `json:"unclassified"` // no capitalizable value
	// CapexTickets etc. count tickets by their effective capitalizable value.
	CapexTickets      int `json:"capex_tickets"`
	OpexTickets       int `json:"opex_tickets"`
	UnclassifiedTicks int `json:"unclassified_tickets"`
}

// Portfolio is an org's projects (the ones the viewer can see) over a range.
type Portfolio struct {
	OrgID    int            `json:"org_id"`
	Since    time.Time      `json:"since"`
	Until    time.Time      `json:"until"`
	Projects []PortfolioRow `json:"projects"`
	Total    PortfolioRow   `json:"total"`
	ByModel  []UsageRow     `json:"usage_by_model"`
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

// OrgPortfolio builds the portfolio for orgID: every project in it that p can
// see, with progress and spend in [since, until), split capex/opex.
func (s *Service) OrgPortfolio(ctx context.Context, p authz.Principal, orgID int, since, until time.Time) (*Portfolio, error) {
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
	out := &Portfolio{OrgID: orgID, Since: since, Until: until, Projects: []PortfolioRow{}, ByModel: []UsageRow{}}
	var allRuns []domain.Run
	for _, proj := range projects {
		if proj.OrgID == nil || *proj.OrgID != orgID || (visible != nil && !visible[proj.ID]) {
			continue
		}
		rep, err := s.ProjectReport(ctx, p, proj.ID, since, until, "capitalizable")
		if err != nil {
			return nil, err
		}
		row := PortfolioRow{ProjectID: proj.ID, Name: proj.Name, Code: proj.Code, State: string(proj.State), Metadata: proj.Metadata,
			Tickets: rep.Tickets, States: rep.States, Completed: rep.Completed, Usage: rep.Usage}
		for _, g := range rep.Breakdown {
			switch g.Key {
			case "true":
				addUsage(&row.Capex, g.Usage)
				row.CapexTickets += g.Tickets
			case "false":
				addUsage(&row.Opex, g.Usage)
				row.OpexTickets += g.Tickets
			default:
				addUsage(&row.Unassigned, g.Usage)
				row.UnclassifiedTicks += g.Tickets
			}
		}
		out.Projects = append(out.Projects, row)
		t := &out.Total
		t.Tickets += row.Tickets
		t.Completed += row.Completed
		t.CapexTickets += row.CapexTickets
		t.OpexTickets += row.OpexTickets
		t.UnclassifiedTicks += row.UnclassifiedTicks
		addUsage(&t.Usage, &row.Usage)
		addUsage(&t.Capex, &row.Capex)
		addUsage(&t.Opex, &row.Opex)
		addUsage(&t.Unassigned, &row.Unassigned)
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
	sort.Slice(out.Projects, func(i, j int) bool {
		return out.Projects[i].Usage.CostUSD > out.Projects[j].Usage.CostUSD ||
			(out.Projects[i].Usage.CostUSD == out.Projects[j].Usage.CostUSD && out.Projects[i].Name < out.Projects[j].Name)
	})
	return out, nil
}
