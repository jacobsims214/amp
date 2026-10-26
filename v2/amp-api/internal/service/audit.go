package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
)

// The audit log records changes to who can do what (orgs, teams, invites,
// project links, roles, accounts, tokens), settings, integrations, and
// deletions. Ticket-level history stays in each ticket's activity log.

var (
	auditMu    sync.RWMutex
	auditSinks = map[int]func(domain.AuditEntry){}
	auditNext  int
)

// OnAudit registers fn to receive every audit entry after it is stored
// (webhooks use this), and returns a function that unregisters it. fn must
// not block.
func OnAudit(fn func(domain.AuditEntry)) (unregister func()) {
	auditMu.Lock()
	defer auditMu.Unlock()
	auditNext++
	id := auditNext
	auditSinks[id] = fn
	return func() {
		auditMu.Lock()
		defer auditMu.Unlock()
		delete(auditSinks, id)
	}
}

// audit records an entry made by p. Recording is best effort: the change
// it describes has already happened, so a failure is logged, not returned.
func (s *Service) audit(ctx context.Context, p authz.Principal, e domain.AuditEntry) {
	_ = s.writeAudit(ctx, p, e)
}

// writeAudit is audit that also returns the failure, for the few actions that
// must not happen without their audit entry (remote.enable, remote.pair).
func (s *Service) writeAudit(ctx context.Context, p authz.Principal, e domain.AuditEntry) error {
	e.ActorUserID = p.UserRef()
	e.ActorEmail = p.Email
	e.ViaToken = p.ViaToken
	fillAuditContext(ctx, p, &e)
	// How the change arrived (ui, mcp, token): approvals are only valid
	// from the web UI, and the log shows which channel each one used.
	if p.Channel != "" {
		if e.Details == nil {
			e.Details = map[string]any{}
		}
		if _, set := e.Details["channel"]; !set {
			e.Details["channel"] = p.Channel
		}
	}
	if s.RelayAudit {
		// The function fills email, org, project, target and summary from
		// the session row, and queues the entry for amp-api's webhooks.
		sessionID := ""
		if e.TargetType == "remote_session" {
			sessionID = e.TargetID
		}
		if _, err := s.Repo.InsertRelayAudit(context.WithoutCancel(ctx), e.ActorUserID, sessionID, e.Action, e.Details); err != nil {
			slog.Error("audit log write failed", "action", e.Action, "target", e.TargetType+":"+e.TargetID, "err", err)
			return err
		}
		return nil
	}
	if e.ProjectID != nil && e.OrgID == nil {
		if proj, err := s.Repo.GetProject(ctx, *e.ProjectID); err == nil {
			e.OrgID = proj.OrgID
		}
	}
	stored, err := s.Repo.InsertAudit(context.WithoutCancel(ctx), e)
	if err != nil {
		slog.Error("audit log write failed", "action", e.Action, "target", e.TargetType+":"+e.TargetID, "err", err)
		return err
	}
	auditMu.RLock()
	defer auditMu.RUnlock()
	for _, fn := range auditSinks {
		fn(*stored)
	}
	return nil
}

// Record adds an audit entry for a change made outside the service (project
// lifecycle handlers).
func (s *Service) Record(ctx context.Context, p authz.Principal, e domain.AuditEntry) {
	s.audit(ctx, p, e)
}

func intRef(n int) *int { return &n }

func idStr(n int) string { return fmt.Sprint(n) }

// Audit lists audit entries the caller may read: global admins any; org
// owners and admins their org's (f.OrgID required); project owners their
// project's (f.ProjectID required).
func (s *Service) Audit(ctx context.Context, p authz.Principal, f domain.AuditFilter) ([]domain.AuditEntry, error) {
	switch {
	case p.Super:
	case f.OrgID != 0:
		if _, err := s.Authz.RequireOrgAdmin(ctx, p, f.OrgID); err != nil {
			return nil, err
		}
	case f.ProjectID != 0:
		if _, err := s.Authz.RequireProject(ctx, p, f.ProjectID, authz.Owner); err != nil {
			return nil, err
		}
	default:
		return nil, authz.Forbidden("the full audit log is for global admins; pass an org you administer or a project you own")
	}
	return s.Repo.ListAudit(ctx, f)
}
