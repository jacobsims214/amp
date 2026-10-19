package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/simstech/amp-api/internal/actor"
)

// Every tool call gets a deadline, and the caller always gets an answer by
// then: a stuck actor, a slow knowledge base or a wedged dependency becomes a
// clear error the agent can act on, not a call that hangs until the client
// gives up (opencode's default is 12 hours). Panics become errors too.

// DefaultToolDeadline bounds most tools; the ones below need longer.
const DefaultToolDeadline = 60 * time.Second

var toolDeadlines = map[string]time.Duration{
	"amp_kb_reindex":            5 * time.Minute,
	"amp_import_project":        3 * time.Minute,
	"amp_export_project":        2 * time.Minute,
	"amp_design_system_publish": 2 * time.Minute,
	"amp_kb_write":              90 * time.Second,
	"amp_kb_search":             90 * time.Second,
	"amp_design_context":        90 * time.Second,
	"amp_publish_artifact":      90 * time.Second,
	"amp_create_tasks":          90 * time.Second,
}

// ToolDeadline is how long a tool may run before AMP answers with a timeout.
func ToolDeadline(name string) time.Duration {
	if d, ok := toolDeadlines[name]; ok {
		return d
	}
	return DefaultToolDeadline
}

// readOnly reports whether a tool only reads, so a timeout is safe to retry.
func readOnly(name string) bool {
	for _, p := range []string{"amp_get_", "amp_list_", "amp_kb_search", "amp_kb_get", "amp_kb_list", "amp_kb_status",
		"amp_kb_tags", "amp_kb_sources", "amp_guide", "amp_whoami", "amp_check_plan", "amp_trace", "amp_velocity",
		"amp_find_by_metadata", "amp_design_context", "amp_audit_log", "amp_export_project", "amp_day_report"} {
		if name == p || strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Middleware applies the deadline and recovery to every tool.
func Middleware() server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			name := req.Params.Name
			limit := ToolDeadline(name)
			ctx, cancel := context.WithTimeout(ctx, limit)
			defer cancel()
			type outcome struct {
				res *mcpgo.CallToolResult
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				defer func() {
					if p := recover(); p != nil {
						slog.Error("mcp tool panicked", "tool", name, "panic", p, "stack", string(debug.Stack()))
						done <- outcome{err: fmt.Errorf("%s failed inside AMP (an internal error, logged for the operators); it's safe to retry once", name)}
					}
				}()
				res, err := next(ctx, req)
				done <- outcome{res, err}
			}()
			select {
			case o := <-done:
				if o.err != nil && (errors.Is(o.err, actor.ErrNoReply) || errors.Is(o.err, context.DeadlineExceeded)) {
					return nil, timeoutError(name, limit)
				}
				return o.res, o.err
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					slog.Warn("mcp tool timed out", "tool", name, "after", limit)
					return nil, timeoutError(name, limit)
				}
				return nil, ctx.Err() // the client cancelled
			}
		}
	}
}

func timeoutError(name string, limit time.Duration) error {
	if readOnly(name) {
		return fmt.Errorf("%s timed out after %s; it only reads, so retry it once, and if it times out again, report it and continue without it", name, limit)
	}
	return fmt.Errorf("%s timed out after %s and may still complete: check the result (e.g. amp_get_task) before retrying, so you don't do it twice", name, limit)
}
