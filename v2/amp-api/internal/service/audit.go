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
	auditSinks []func(domain.AuditEntry)
)

// OnAudit registers fn to receive every audit entry after it is stored
// (webhooks use this). fn must not block.
func OnAudit(fn func(domain.AuditEntry)) {
	auditMu.Lock()
	defer auditMu.Unlock()
	auditSinks = append(auditSinks, fn)
}

// audit records an entry made by p. Recording is best effort: the change
// it describes has already happened, so a failure is logged, not returned.
func (s *Service) audit(ctx context.Context, p authz.Principal, e domain.AuditEntry) {
	e.ActorUserID = p.UserRef()
	e.ActorEmail = p.Email
	e.ViaToken = p.ViaToken
	if e.ProjectID != nil && e.OrgID == nil {
		if proj, err := s.Repo.GetProject(ctx, *e.ProjectID); err == nil {
			e.OrgID = proj.OrgID
		}
	}
	stored, err := s.Repo.InsertAudit(context.WithoutCancel(ctx), e)
	if err != nil {
		slog.Error("audit log write failed", "action", e.Action, "target", e.TargetType+":"+e.TargetID, "err", err)
		return
	}
	auditMu.RLock()
	defer auditMu.RUnlock()
	for _, fn := range auditSinks {
		fn(*stored)
	}
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
