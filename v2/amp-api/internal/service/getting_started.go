package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/simstech/amp-api/internal/authz"
)

// The Get started checklist on an empty My work (#128). Every step is read
// from what the server already knows, so a step stays checked once it's
// done, on every device: an agent reached AMP as you, you can see a
// project, and you approved a plan.
//
// The fourth step, "See your first receipt", is added by whichever of this
// change (#128) and verification receipts (#149) lands second.

// GettingStartedStep is one step: whether it's done, since when, and the
// project it happened in when that's useful for a link.
type GettingStartedStep struct {
	Done      bool       `json:"done"`
	At        *time.Time `json:"at,omitempty"`
	ProjectID int        `json:"project_id,omitempty"`
	// Client is how the agent connected (mcp, harness, token); agent step only.
	Client string `json:"client,omitempty"`
}

// GettingStarted is GET /api/my-work/getting-started.
type GettingStarted struct {
	Agent   GettingStartedStep `json:"agent"`
	Project GettingStartedStep `json:"project"`
	Plan    GettingStartedStep `json:"plan"`
}

// agentContactKey names the caller in agent_contacts: their AMP user, or
// local-dev when sign-in is off and there's no user.
func agentContactKey(p authz.Principal) string {
	if p.UserID != 0 {
		return fmt.Sprintf("user:%d", p.UserID)
	}
	return "local-dev"
}

// contactSeen keeps a busy agent from writing its contact on every call: one
// write a minute per caller and process (the row's own guard is the same).
var contactSeen = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// NoteAgentContact records that p reached AMP over MCP. It never holds up
// or fails the caller's request: the write runs in the background and
// errors are logged (and retried after a short pause, not on every call).
func (s *Service) NoteAgentContact(ctx context.Context, p authz.Principal) {
	key := agentContactKey(p)
	now := time.Now()
	contactSeen.Lock()
	if until, ok := contactSeen.at[key]; ok && now.Before(until) {
		contactSeen.Unlock()
		return
	}
	if len(contactSeen.at) > 10000 {
		contactSeen.at = map[string]time.Time{}
	}
	contactSeen.at[key] = now.Add(time.Minute)
	contactSeen.Unlock()

	client := auditClient(auditActorType(p, p.Req), p.AuthMethod, p.Req)
	go func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := s.Repo.NoteAgentContact(ctx, key, p.UserRef(), p.ManagedBy, client); err != nil {
			slog.Warn("record agent contact", "principal", key, "err", err)
			contactSeen.Lock()
			contactSeen.at[key] = time.Now().Add(10 * time.Second) // try again soon, not on every call
			contactSeen.Unlock()
		}
	}(context.WithoutCancel(ctx))
}

// GettingStarted reads p's checklist.
func (s *Service) GettingStarted(ctx context.Context, p authz.Principal) (*GettingStarted, error) {
	out := &GettingStarted{}
	contact, err := s.Repo.FirstAgentContact(ctx, agentContactKey(p), p.UserID)
	if err != nil {
		return nil, err
	}
	if contact != nil {
		at := contact.At
		out.Agent = GettingStartedStep{Done: true, At: &at, Client: contact.Client}
	}

	ids, err := s.visibleIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	start, err := s.Repo.ProjectStartFor(ctx, ids, p.UserID)
	if err != nil {
		return nil, err
	}
	if start.FirstProjectID != 0 {
		out.Project = GettingStartedStep{Done: true, At: start.CreatedAt, ProjectID: start.FirstProjectID}
	}
	if start.PlanProjectID != 0 {
		out.Plan = GettingStartedStep{Done: true, At: start.PlanApprovedAt, ProjectID: start.PlanProjectID}
	} else {
		out.Plan.ProjectID = start.FirstProjectID
	}

	return out, nil
}
