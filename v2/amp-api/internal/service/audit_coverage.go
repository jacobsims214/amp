package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/reqctx"
)

// Audit log v2 coverage (KB decisions/audit-log-v2.md, ticket #2428): the
// security-relevant actions the log didn't record before. Refusals use
// distinct action names (plan.approve.denied, access.denied, ...) with
// outcome=denied, so a consumer that ignores outcome never reads a refusal
// as the change, and are rate-limited by auditDenied.

// ---- Authentication (auth.Verifier.OnEvent) ----

// tokenIPWindow is how long an address stays known for an API token: a use
// from an address with no audit row for the token in this window is
// recorded as token.use.new_ip.
const tokenIPWindow = 30 * 24 * time.Hour

// seenCache remembers keys (Dex sessions, token+address pairs) already
// handled, so the per-request auth events reach the database only on a
// miss. Bounded: past maxSeen keys, expired ones are dropped, and if none
// are, the whole cache is (the database check stays authoritative).
type seenCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	keys map[string]time.Time
}

const maxSeen = 50000

func newSeenCache(ttl time.Duration) *seenCache {
	return &seenCache{ttl: ttl, keys: map[string]time.Time{}}
}

// claim reports whether key is new (not seen within ttl) and marks it
// seen, so concurrent requests of one session handle it once.
func (c *seenCache) claim(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if until, ok := c.keys[key]; ok && now.Before(until) {
		return false
	}
	if len(c.keys) >= maxSeen {
		for k, until := range c.keys {
			if now.After(until) {
				delete(c.keys, k)
			}
		}
		if len(c.keys) >= maxSeen {
			c.keys = map[string]time.Time{}
		}
	}
	c.keys[key] = now.Add(c.ttl)
	return true
}

// forget unmarks key (its handling failed, so a later request retries).
func (c *seenCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.keys, key)
}

var (
	loginSeen   = newSeenCache(24 * time.Hour)
	tokenIPSeen = newSeenCache(time.Hour)
)

// RecordAuthEvent writes the audit rows for an authentication event (wired
// to auth.Verifier.OnEvent in main):
//   - auth.login once per Dex sign-in, checked across replicas in the
//     database. The session id is stable across token refreshes when the
//     token has auth_time or a nonce (auth.DexSessionID; oauth2-proxy sends
//     a nonce), so oauth2-proxy's hourly refresh is not audited: it is the
//     same sign-in (#2440). A token with neither still counts per iat;
//   - user.create when a first sign-in creates the account, and
//     invite.accept for each pending invite it accepted, with the roles
//     granted;
//   - token.use.new_ip the first time an API token is used from an address
//     with no audit row for it in 30 days;
//   - auth.token.fail and auth.signup.refused (anonymous, rate-limited);
//   - access.denied for a deactivated account's request.
func (s *Service) RecordAuthEvent(ctx context.Context, ev auth.Event) {
	p := s.authEventPrincipal(ctx, ev)
	switch ev.Kind {
	case auth.EventLogin:
		s.recordLogin(ctx, p, ev)
	case auth.EventProvisioned:
		s.recordProvisioned(ctx, p, ev)
	case auth.EventTokenUse:
		s.recordTokenUse(ctx, p)
	case auth.EventTokenFail:
		s.auditDenied(ctx, p, auth.EventTokenFail, domain.AuditEntry{TargetType: "token",
			Summary: "an API token was rejected (" + ev.TokenPrefix + "...)", Details: map[string]any{"token_prefix": ev.TokenPrefix}},
			errors.New(ev.Reason))
	case auth.EventSignupRefused:
		s.auditDenied(ctx, p, auth.EventSignupRefused, domain.AuditEntry{TargetType: "user", TargetName: ev.Email,
			Summary: "refused to create an AMP account for " + ev.Email, Details: map[string]any{"claimed_email": ev.Email}},
			errors.New(ev.Reason))
	case auth.EventDeactivated:
		s.auditDenied(ctx, p, "access", domain.AuditEntry{TargetType: "account", TargetID: idStr(p.UserID), TargetName: p.Email,
			Summary: "refused a request from deactivated account " + p.Email}, errors.New(ev.Reason))
	}
}

// authEventPrincipal is who an auth event is about: the authenticated user,
// or for a refusal before there was one, an anonymous caller (with the
// email the token claimed, if any).
func (s *Service) authEventPrincipal(ctx context.Context, ev auth.Event) authz.Principal {
	if ev.Ident != nil && ev.Ident.User != nil {
		if p, err := authz.FromContext(auth.WithIdentity(ctx, ev.Ident)); err == nil {
			return p
		}
	}
	req, _ := reqctx.FromContext(ctx)
	return authz.Principal{Email: ev.Email, Req: req}
}

func (s *Service) recordLogin(ctx context.Context, p authz.Principal, ev auth.Event) {
	sid := ev.Ident.SessionID
	if !loginSeen.claim(sid, time.Now()) {
		return
	}
	seen, err := s.Repo.AuditLoginSeen(ctx, sid)
	if err != nil {
		loginSeen.forget(sid)
		slog.Warn("audit: couldn't check for an earlier sign-in of this session", "err", err)
		return
	}
	if seen {
		return
	}
	s.audit(ctx, p, domain.AuditEntry{Action: auth.EventLogin, TargetType: "user", TargetID: idStr(p.UserID), TargetName: p.Email,
		Summary: p.Email + " signed in (new Dex session)", Details: map[string]any{"listener": ev.Ident.Listener}})
}

func (s *Service) recordProvisioned(ctx context.Context, p authz.Principal, ev auth.Event) {
	pv := ev.Provisioned
	if pv.Created {
		s.audit(ctx, p, domain.AuditEntry{Action: "user.create", TargetType: "user", TargetID: idStr(p.UserID), TargetName: p.Email,
			Summary: "created AMP account " + p.Email + " at first sign-in, as " + pv.InitialRole,
			Details: map[string]any{"initial_role": pv.InitialRole, "invites_accepted": len(pv.Invites)},
			After:   map[string]any{"roles": []any{pv.InitialRole}}})
	}
	for _, inv := range pv.Invites {
		s.auditInviteAccept(ctx, p, p.UserID, p.Email, inv, "at sign-in")
	}
}

// auditInviteAccept records that inv was accepted for user userID: the org
// role it granted and, if it names a team, the team role.
func (s *Service) auditInviteAccept(ctx context.Context, p authz.Principal, userID int, email string, inv domain.Invite, how string) {
	after := map[string]any{"org_id": inv.OrgID, "org_role": inv.OrgRole}
	granted := "org " + inv.OrgRole
	if inv.TeamID != nil {
		after["team_id"], after["team_role"] = *inv.TeamID, inv.TeamRole
		granted += ", team " + inv.TeamRole
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "invite.accept", OrgID: intRef(inv.OrgID), TargetType: "invite", TargetID: idStr(inv.ID),
		TargetName: email, Summary: fmt.Sprintf("invite for %s accepted %s: granted %s", email, how, granted),
		Details: map[string]any{"user_id": userID, "invited_by": inv.InvitedBy}, After: after})
}

func (s *Service) recordTokenUse(ctx context.Context, p authz.Principal) {
	if p.TokenID == 0 || p.Req == nil || !p.Req.SourceIP.IsValid() {
		return
	}
	ip := p.Req.SourceIP.String()
	key := fmt.Sprintf("%d|%s", p.TokenID, ip)
	if !tokenIPSeen.claim(key, time.Now()) {
		return
	}
	seen, err := s.Repo.AuditTokenIPSeen(ctx, p.TokenID, ip, time.Now().Add(-tokenIPWindow))
	if err != nil {
		tokenIPSeen.forget(key)
		slog.Warn("audit: couldn't check the token's known addresses", "err", err)
		return
	}
	if seen {
		return
	}
	s.audit(ctx, p, domain.AuditEntry{Action: "token.use.new_ip", TargetType: "token", TargetID: fmt.Sprint(p.TokenID), TargetName: p.Email,
		Summary: "API token of " + p.Email + " used from an address not seen for it in 30 days"})
}

// ---- Refusals ----

// AccessScope names what a refused request was aimed at, as far as the
// listener can tell from the route or the tool's arguments (0: not given).
// AuditAccessDenied resolves it to the org and project the row is filed
// under, so org admins and org/project webhooks see refused attempts on
// their resources (#2440, review #2432 L5).
type AccessScope struct {
	ProjectID, OrgID, TaskID, EpicID, StoryID, TeamID int
}

// AuditAccessDenied records a request a listener refused with 403
// (access.denied, rate-limited per actor and target), unless a specific
// refusal (plan.approve.denied, task.dispatch.denied, ...) was already
// recorded for it. surface is "route" (REST: method and path) or "tool"
// (MCP: the tool name), op the route or tool, and scope what it targeted.
func (s *Service) AuditAccessDenied(ctx context.Context, p authz.Principal, surface, op string, scope AccessScope, err error) {
	req := p.Req
	if req == nil {
		req, _ = reqctx.FromContext(ctx)
	}
	if req.DeniedAudited() {
		return
	}
	e := domain.AuditEntry{TargetType: surface, TargetID: reqctx.Clip(op, 200),
		Summary: "refused " + p.Label() + ": " + reqctx.Clip(op, 200)}
	e.ProjectID, e.OrgID = s.resolveAccessScope(ctx, scope)
	s.auditDenied(ctx, p, "access", e, err)
}

// resolveAccessScope returns the project and org scope points at, each nil
// when it names none or doesn't exist (a made-up id files nothing under
// anyone). A ticket, epic or story gives its project; a team its org; a
// project's org is filled in by audit.
func (s *Service) resolveAccessScope(ctx context.Context, sc AccessScope) (projectID, orgID *int) {
	if s.Repo == nil {
		return nil, nil
	}
	pid := sc.ProjectID
	switch {
	case pid != 0:
	case sc.TaskID != 0:
		if t, err := s.Repo.GetTask(ctx, sc.TaskID); err == nil {
			pid = t.ProjectID
		}
	case sc.StoryID != 0:
		if st, err := s.Repo.GetStory(ctx, sc.StoryID); err == nil {
			pid = st.ProjectID
		}
	case sc.EpicID != 0:
		if ep, err := s.Repo.GetEpic(ctx, sc.EpicID); err == nil {
			pid = ep.ProjectID
		}
	}
	if pid != 0 {
		if proj, err := s.Repo.GetProject(ctx, pid); err == nil {
			projectID, orgID = intRef(proj.ID), proj.OrgID
		}
	}
	if orgID == nil {
		oid := sc.OrgID
		if oid == 0 && sc.TeamID != 0 {
			if tm, err := s.Repo.GetTeam(ctx, sc.TeamID); err == nil {
				oid = tm.OrgID
			}
		}
		if oid != 0 {
			if org, err := s.Repo.GetOrg(ctx, oid); err == nil {
				orgID = intRef(org.ID)
			}
		}
	}
	return projectID, orgID
}

// AuditApproveDenied records a refused plan approval (plan.approve.denied):
// an agent, an MCP call (amp_approve_plan) or an API token trying to
// approve, or approving others' tickets without on_behalf or lead access.
// others are the requested tickets that weren't the caller's, if that was
// the reason.
func (s *Service) AuditApproveDenied(ctx context.Context, p authz.Principal, projectID int, req ApproveRequest, others []int, err error) {
	details := map[string]any{"on_behalf": req.OnBehalf}
	if req.TaskIDs != nil {
		details["task_ids"] = req.TaskIDs
	}
	if req.AsOf != nil {
		details["as_of"] = req.AsOf.UTC().Format(time.RFC3339Nano)
	}
	if len(others) > 0 {
		details["others"] = others
	}
	e := domain.AuditEntry{ProjectID: intRef(projectID), TargetType: "project", TargetID: idStr(projectID),
		Summary: fmt.Sprintf("refused plan approval on project %d by %s", projectID, p.Label()), Details: details}
	if proj, perr := s.Repo.GetProject(ctx, projectID); perr == nil {
		e.TargetName = proj.Name
		e.Summary = "refused plan approval on " + proj.Name + " by " + p.Label()
	}
	s.auditDenied(ctx, p, "plan.approve", e, err)
}

// auditTaskDenied records a gate refusing to move ticket t forward: action
// is task.dispatch, task.complete, task.state or task.human.complete.
func (s *Service) auditTaskDenied(ctx context.Context, p authz.Principal, action string, t *domain.Task, err error, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	details["state"] = string(t.State)
	verb := map[string]string{"task.dispatch": "dispatch", "task.complete": "completion", "task.state": "state change",
		"task.human.complete": "marking done"}[action]
	s.auditDenied(ctx, p, action, domain.AuditEntry{ProjectID: intRef(t.ProjectID), TargetType: "task", TargetID: idStr(t.ID), TargetName: t.Name,
		Summary: fmt.Sprintf("refused %s of ticket %d by %s", verb, t.ID, p.Label()), Details: details}, err)
}

// ---- Knowledge base ----

// AuditKB records a knowledge base change: kb.delete, kb.restore and
// kb.reindex in any KB, kb.write in a team or org KB only (project KB
// writes are routine and stay in the doc's own history).
func (s *Service) AuditKB(ctx context.Context, p authz.Principal, action string, k *KB, path string, details map[string]any) {
	if k == nil || (action == "kb.write" && k.Kind == kb.ScopeProject) {
		return
	}
	e := domain.AuditEntry{Action: action, TargetType: "kb_doc", TargetID: reqctx.Clip(k.Kind+":"+idStr(k.ID)+":"+path, 500),
		TargetName: k.Name, Details: map[string]any{"scope": k.Kind, "scope_id": k.ID, "path": path}}
	for key, v := range details {
		e.Details[key] = v
	}
	switch k.Kind {
	case kb.ScopeProject:
		e.ProjectID = intRef(k.ID)
	case kb.ScopeOrg:
		e.OrgID = intRef(k.ID)
	case kb.ScopeTeam:
		if team, err := s.Repo.GetTeam(ctx, k.ID); err == nil {
			e.OrgID = intRef(team.OrgID)
		}
	}
	where := fmt.Sprintf("the %s %s KB", k.Name, k.Kind)
	switch action {
	case "kb.delete":
		e.Summary = "deleted " + path + " from " + where
	case "kb.restore":
		e.Summary = "restored " + path + " in " + where
	case "kb.write":
		e.Summary = "wrote " + path + " in " + where
	case "kb.reindex":
		e.TargetType, e.TargetID = "kb", k.Kind+":"+idStr(k.ID)
		e.Summary = "started a reindex of " + where
	}
	s.audit(ctx, p, e)
}

// ---- Exports ----

// RecordProjectExport records a whole-project export (project.export): the
// bundle holds every ticket, comment and KB doc, so who took it and from
// where is security-relevant.
func (s *Service) RecordProjectExport(ctx context.Context, p authz.Principal, projectID int, b *domain.ExportBundle) {
	e := domain.AuditEntry{Action: "project.export", ProjectID: intRef(projectID), TargetType: "project", TargetID: idStr(projectID),
		Summary: fmt.Sprintf("exported project %d", projectID)}
	if b != nil {
		e.TargetName = b.Project.Name
		e.Summary = "exported project " + b.Project.Name
		e.Details = map[string]any{"epics": len(b.Epics), "stories": len(b.Stories), "tasks": len(b.Tasks), "kb_docs": len(b.KBDocs)}
	}
	s.audit(ctx, p, e)
}

// RecordAuditExport records an export of the audit log itself
// (audit.export) with its filter and row count. Listing the log stays
// unaudited; the export endpoint (#2431) calls this.
func (s *Service) RecordAuditExport(ctx context.Context, p authz.Principal, f domain.AuditFilter, format string, rows int) {
	e := domain.AuditEntry{Action: "audit.export", TargetType: "audit_log",
		Summary: fmt.Sprintf("exported %d audit log row(s) as %s", rows, format),
		Details: map[string]any{"format": format, "rows": rows, "filter": map[string]any{
			"org_id": f.OrgID, "project_id": f.ProjectID, "actor_user_id": f.ActorUserID, "action": f.Action, "before_id": f.BeforeID}}}
	if f.OrgID != 0 {
		e.OrgID = intRef(f.OrgID)
	}
	if f.ProjectID != 0 {
		e.ProjectID = intRef(f.ProjectID)
	}
	s.audit(ctx, p, e)
}
