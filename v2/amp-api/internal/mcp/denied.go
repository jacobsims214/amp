package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/authz"
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
				s.svc.AuditAccessDenied(ctx, p, "tool", req.Params.Name, err)
			}
			return res, err
		}
	}
}
