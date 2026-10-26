package domain

import (
	"fmt"
	"strings"
	"time"
)

// Webhook POSTs AMP events to a URL: for one project, or for every project
// in an org (which can also receive the org's audit entries).
type Webhook struct {
	ID        int    `json:"id"`
	OrgID     *int   `json:"org_id,omitempty"`
	ProjectID *int   `json:"project_id,omitempty"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Format    string `json:"format"` // WebhookFormatJSON or WebhookFormatSlack
	// Secret signs each delivery (X-AMP-Signature). Only returned when a
	// webhook is created or its secret rotated.
	Secret string   `json:"secret,omitempty"`
	Events []string `json:"events"` // patterns, see WebhookMatches
	Active bool     `json:"active"`
	// IncludeContext opts an audit webhook into the request context of
	// each entry: source_ip, user_agent, session_id, token_id, before and
	// after (personal data, audit log v2). Off by default, and for every
	// webhook made before v2 (#2439 M2).
	IncludeContext      bool       `json:"include_context"`
	CreatedBy           *int       `json:"created_by,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	LastDeliveryAt      *time.Time `json:"last_delivery_at,omitempty"`
	LastStatus          *int       `json:"last_status,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
}

const (
	WebhookFormatJSON  = "json"  // the AMP event envelope, signed
	WebhookFormatSlack = "slack" // {"text": ...} for Slack (or compatible) incoming webhooks
)

// WebhookDelivery is one POST of one event to one webhook, with its retries.
type WebhookDelivery struct {
	ID            int64      `json:"id"`
	WebhookID     int        `json:"webhook_id"`
	EventType     string     `json:"event_type"`
	Body          string     `json:"body,omitempty"`
	Status        string     `json:"status"` // pending | delivered | failed
	Attempts      int        `json:"attempts"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	LastStatus    *int       `json:"last_status,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	DeliveredAt   *time.Time `json:"delivered_at,omitempty"`
}

// EventPing is sent by "test webhook".
const EventPing EventType = "ping"

// webhookGroups are the event families a pattern like "task.*" can name.
var webhookGroups = []string{"project", "epic", "story", "task", "comment", "audit"}

// ValidateWebhookEvents checks event patterns: "*" (everything, audit
// entries included), "<group>.*", an exact event type, or for audit
// entries "audit.<action>" (e.g. "audit.org.member.set").
func ValidateWebhookEvents(patterns []string) ([]string, error) {
	known := map[EventType]bool{EventPing: true}
	for _, t := range AllEventTypes {
		known[t] = true
	}
	out := []string{}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
			continue
		case p == "*", known[EventType(p)], strings.HasPrefix(p, "audit.") && len(p) > len("audit."):
		case strings.HasSuffix(p, ".*") && contains(webhookGroups, strings.TrimSuffix(p, ".*")):
		default:
			return nil, fmt.Errorf("unknown event %q: use \"*\", a group like \"task.*\" (%s), an event like \"task.completed\", or \"audit.*\"",
				p, strings.Join(webhookGroups, ", "))
		}
		out = append(out, p)
	}
	return out, nil
}

// WebhookMatches reports whether a webhook with these patterns wants an
// event of type t. No patterns means every project event, but no audit
// entries: those are opted into ("audit.*" or "*").
func WebhookMatches(patterns []string, t string) bool {
	if t == string(EventPing) {
		return true
	}
	audit := strings.HasPrefix(t, "audit.")
	if len(patterns) == 0 {
		return !audit
	}
	for _, p := range patterns {
		switch {
		case p == "*", p == t:
			return true
		case strings.HasSuffix(p, ".*") && strings.HasPrefix(t, strings.TrimSuffix(p, "*")):
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
