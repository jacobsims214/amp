package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/simstech/amp-api/internal/domain"
)

// slackText is one line describing an event, for Slack (or any service
// taking {"text": ...}). Slack mrkdwn: *bold*, and <, >, & escaped.
func (d *Dispatcher) slackText(ctx context.Context, env Envelope, it item) string {
	if it.audit != nil {
		a := it.audit
		who := a.ActorEmail
		if who == "" {
			who = "AMP"
		}
		where := ""
		if env.ProjectID != 0 {
			where = " in *" + esc(d.project(ctx, env.ProjectID).name) + "*"
		}
		return fmt.Sprintf("🔐 %s %s%s", esc(who), esc(a.Summary), where)
	}
	if env.Type == string(domain.EventPing) {
		return "👋 Test message from AMP: this webhook works."
	}
	f := fields(env.Data)
	project := "*" + esc(d.project(ctx, env.ProjectID).name) + "*"
	ticket := fmt.Sprintf("#%s %s", f.str("id"), esc(f.str("name")))
	switch domain.EventType(env.Type) {
	case domain.EventTaskCreated:
		return fmt.Sprintf("🆕 %s created in %s", ticket, project)
	case domain.EventTaskDispatched:
		by := f.str("agent_id")
		if by != "" {
			by = " by " + esc(by)
		}
		return fmt.Sprintf("▶️ %s started%s in %s", ticket, by, project)
	case domain.EventTaskCompleted:
		return fmt.Sprintf("✅ %s completed in %s", ticket, project)
	case domain.EventTaskBlocked:
		why := f.str("block_reason")
		if why != "" {
			why = ": " + esc(truncate(why, 200))
		}
		return fmt.Sprintf("⛔ %s blocked in %s%s", ticket, project, why)
	case domain.EventTaskUnblocked:
		return fmt.Sprintf("🔓 %s is ready in %s", ticket, project)
	case domain.EventTaskAssigned:
		return fmt.Sprintf("👤 %s reassigned in %s", ticket, project)
	case domain.EventTaskUpdated:
		if f.str("deleted") == "true" {
			return fmt.Sprintf("🗑️ ticket #%s deleted in %s", f.str("task_id"), project)
		}
		return fmt.Sprintf("✏️ %s updated in %s", ticket, project)
	case domain.EventCommentAdded:
		return fmt.Sprintf("💬 %s on #%s in %s: %s", esc(f.str("author")), f.str("task_id"), project, esc(truncate(f.str("body"), 280)))
	}
	// Projects, epics, stories: "<kind> <name> <what>".
	kind, what, _ := strings.Cut(env.Type, ".")
	what = strings.ReplaceAll(what, "_", " ")
	if s := f.str("state"); what == "state changed" && s != "" {
		what = "is now " + strings.ReplaceAll(s, "_", " ")
	}
	name := f.str("name")
	if name == "" {
		return fmt.Sprintf("%s %s in %s", kind, what, project)
	}
	return fmt.Sprintf("%s *%s* %s in %s", kind, esc(name), what, project)
}

// payloadFields reads an event payload's top-level values as strings.
type payloadFields map[string]any

func fields(v any) payloadFields {
	b, _ := json.Marshal(v)
	m := payloadFields{}
	_ = json.Unmarshal(b, &m)
	return m
}

func (f payloadFields) str(k string) string {
	switch v := f[k].(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return fmt.Sprint(int64(v))
	default:
		return fmt.Sprint(v)
	}
}

func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
