package mcp

import (
	"context"
	"strconv"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/service"
)

// deniedMiddleware records every tool call refused with 403 as
// access.denied (rate-limited per actor and tool; KB
// decisions/audit-log-v2.md, #2428), unless the service already recorded
// a specific refusal for the request (plan.approve.denied,
// task.dispatch.denied, ...). A project the caller can't see answers 404
// and isn't recorded: AMP doesn't confirm it exists.
func (s *Server) deniedMiddleware() server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			res, err := next(ctx, req)
			if err == nil || authz.StatusOf(err, 0) != 403 {
				return res, err
			}
			if p, perr := authz.FromContext(ctx); perr == nil {
				s.svc.AuditAccessDenied(ctx, p, "tool", req.Params.Name, toolScope(req.GetArguments()), err)
			}
			return res, err
		}
	}
}

// toolScope reads what a tool call targets from its arguments (project_id,
// org_id, task_id, epic_id, story_id, team_id), so its access.denied row is
// filed under that org or project.
func toolScope(args map[string]any) service.AccessScope {
	id := func(k string) int {
		switch v := args[k].(type) {
		case float64:
			if v > 0 && v == float64(int(v)) {
				return int(v)
			}
		case string:
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
		return 0
	}
	return service.AccessScope{ProjectID: id("project_id"), OrgID: id("org_id"), TaskID: id("task_id"),
		EpicID: id("epic_id"), StoryID: id("story_id"), TeamID: id("team_id")}
}
