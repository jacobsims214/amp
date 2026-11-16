package mcp

// The MCP boundary for submit-only service credentials (AMP #3772; approved
// contract artifact 36 v2 §5, build slice B5).
//
// A submit-scoped API token may call exactly one tool: `amp_submit_request`,
// and only with the team it is bound to. Every other tool is refused before its
// handler runs, by a server-wide tool middleware rather than inside each
// handler — so a tool added to this package later is refused by default
// instead of quietly inheriting the account's authority.
//
// Two consequences worth stating, because both are the point:
//
//   - The tool list a scoped credential sees is trimmed to the one tool it can
//     use (listFilter), so a client reading tools/list is told the truth instead
//     of discovering the scope one 403 at a time. The middleware still refuses
//     the rest: a hidden tool stays callable in mcp-go, and the refusal must not
//     depend on the client having read the list.
//   - The capability layer narrows the same credential independently
//     (authz/token_scope.go: no project level, no org role, no inbox read, an
//     empty visible-project set, and TeamSubmit only for the bound team). As on
//     the REST surface, one layer decides what the credential may *do* and the
//     other what it may *see*; neither depends on the other being present.
//
// Nothing here invents a second definition of "submit". The tool's own handler
// runs the same service path as REST, and its authorization is Checker.TeamSubmit
// on the team it names, which for a scoped credential passes only for the bound
// team and only while the account could submit there anyway.

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/authz"
)

// SubmitScopeTool is the one tool a submit-only credential may call: the same
// submission REST exposes as POST /api/teams/{id}/requests.
const SubmitScopeTool = "amp_submit_request"

// submitScopeGate refuses a submit-scoped credential on every tool but
// SubmitScopeTool. It is a tool middleware so every registered tool passes
// through it; the arguments are looked at only when the caller reached the
// allowed tool at all.
func (s *Server) submitScopeGate() server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			p, err := authz.FromContext(ctx)
			if err == nil && p.SubmitOnly() && req.Params.Name != SubmitScopeTool {
				return nil, authz.TokenScopeForbidden()
			}
			return next(ctx, req)
		}
	}
}

// submitScopeToolNames is the set listFilter keeps for a submit-scoped
// credential. A map rather than a literal comparison so the trimmed list and the
// middleware above can never drift apart silently: the test asserts the two agree.
func submitScopeToolNames() map[string]bool { return map[string]bool{SubmitScopeTool: true} }
