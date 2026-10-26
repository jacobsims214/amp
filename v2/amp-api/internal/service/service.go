// Package service holds the multi-user operations shared by the REST API
// and the MCP tools — project placement, ticket ownership, grouping owners —
// so both surfaces enforce exactly the same rules.
package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/simstech/amp-api/internal/actor"
	"github.com/simstech/amp-api/internal/authz"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/kb"
	"github.com/simstech/amp-api/internal/repository"
)

type Service struct {
	Repo     *repository.Repo
	Registry *actor.Registry
	Authz    *authz.Checker
	KB       *kb.Service // nil when the knowledge base isn't configured
	// AppURL is AMP's public address (https://amp.example.com), for links to
	// pages in the UI. Empty when unknown: links are then left out.
	AppURL string
	// ArtifactRetention is how long an artifact lives after its last version
	// unless someone keeps it (0: the default, 45 days).
	ArtifactRetention time.Duration
	// Remote is the instance's remote control configuration (off unless
	// RELAY_ENABLED and auth are on).
	Remote RemoteConfig
	// OnRemoteEnded, when set, is told about remote sessions the service
	// ended (policy off, revoke-all, expiry), so the relay closes their
	// streams. It must not block.
	OnRemoteEnded func([]domain.RemoteSession)
	// RelayAudit is set in amp-relay (cmd/relay): audit entries go through
	// the database's relay_record_audit function (the relay's role can't
	// insert into audit_log), and amp-api's webhook dispatcher picks them up
	// from there, so no in-process audit sink (OnAudit) sees them (#2399,
	// re-review #2394 N3).
	RelayAudit bool
}

func New(repo *repository.Repo, registry *actor.Registry, kbSvc *kb.Service) *Service {
	return &Service{Repo: repo, Registry: registry, Authz: authz.NewChecker(repo), KB: kbSvc}
}

// dropKB removes a team's or org's knowledge base after the team or org
// itself is gone. Its docs go to the trash in Postgres (restorable with
// kb.Service.RestoreKB, purged after 30 days) and only then is the search
// collection dropped. A failure leaves an orphaned collection or live docs
// of a deleted owner, not a lost doc or a broken request, so it is logged
// rather than returned.
func (s *Service) dropKB(sc kb.Scope) {
	if s.KB == nil {
		return
	}
	if err := s.KB.Drop(context.Background(), sc); err != nil {
		slog.Default().Warn("dropping knowledge base failed", "kb", sc.String(), "err", err)
	}
}

// ---- Projects ----

// ResolveProjectHome decides the org and owner team for a new project.
// Omitted values are inferred when unambiguous: the caller's only org, and
// their only team in it. Org admins (and global admins) may create a project
// without an owner team; anyone else must own it through one of their teams.
func (s *Service) ResolveProjectHome(ctx context.Context, p authz.Principal, orgID, teamID *int) (domain.CreateProjectRequest, error) {
	home := domain.CreateProjectRequest{CreatedBy: p.UserRef()}

	if orgID == nil {
		var uid *int
		if !p.Super {
			uid = p.UserRef()
		}
		orgs, err := s.Repo.ListOrgs(ctx, uid)
		if err != nil {
			return home, err
		}
		if !p.HasUser() {
			// Local dev (auth disabled) is one anonymous user: use the
			// Default org the migration creates, if there's a choice.
			for _, o := range orgs {
				if o.Slug == "default" {
					orgs = []domain.Org{o}
					break
				}
			}
		}
		switch len(orgs) {
		case 0:
			return home, authz.Forbidden("you are not in any org; ask an admin to add you to one")
		case 1:
			id := orgs[0].ID
			orgID = &id
		default:
			return home, authz.BadRequest("org_id is required: you belong to %s", describeOrgs(orgs))
		}
	}
	role, err := s.Authz.RequireOrgMember(ctx, p, *orgID)
	if err != nil {
		return home, err
	}
	home.OrgID = orgID
	orgAdmin := p.Super || role == domain.OrgRoleOwner || role == domain.OrgRoleAdmin

	if teamID != nil {
		team, err := s.Repo.GetTeam(ctx, *teamID)
		if err != nil || team.OrgID != *orgID {
			return home, authz.BadRequest("team %d is not in org %d", *teamID, *orgID)
		}
		if !orgAdmin {
			tr, err := s.Repo.GetTeamRole(ctx, team.ID, p.UserID)
			if err != nil {
				return home, err
			}
			if tr == "" {
				return home, authz.Forbidden("you are not on team %d", team.ID)
			}
		}
		home.TeamID = teamID
		return home, nil
	}

	if !p.HasUser() {
		return home, nil // local dev: no teams to infer from
	}
	teams, err := s.Repo.ListUserTeams(ctx, *orgID, p.UserID)
	if err != nil {
		return home, err
	}
	switch {
	case len(teams) == 1:
		id := teams[0].UserID // ListUserTeams carries the team id in UserID
		home.TeamID = &id
	case len(teams) > 1:
		names := make([]string, len(teams))
		for i, t := range teams {
			names[i] = fmt.Sprintf("%d (%s)", t.UserID, t.Email)
		}
		return home, authz.BadRequest("team_id is required: you are on teams %s", strings.Join(names, ", "))
	case !orgAdmin:
		return home, authz.Forbidden("you are not on any team in org %d; ask an org admin to add you to one", *orgID)
	}
	return home, nil
}

func describeOrgs(orgs []domain.Org) string {
	const max = 10
	var parts []string
	for i, o := range orgs {
		if i == max {
			parts = append(parts, fmt.Sprintf("and %d more", len(orgs)-max))
			break
		}
		parts = append(parts, fmt.Sprintf("%d (%s)", o.ID, o.Name))
	}
	return strings.Join(parts, ", ")
}

// ProjectMembers lists users with access to the project.
func (s *Service) ProjectMembers(ctx context.Context, projectID int) ([]domain.ProjectMember, error) {
	rows, err := s.Repo.ListProjectMembers(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ProjectMember, 0, len(rows))
	for _, m := range rows {
		out = append(out, domain.ProjectMember{
			UserID: m.User.ID, Email: m.User.Email, DisplayName: m.User.DisplayName,
			Access: authz.LevelFromInt(m.Level).String(),
			Kind:   m.User.Kind, Disabled: m.User.DisabledAt != nil,
		})
	}
	return out, nil
}

// requireMember fails unless userID can access projectID — the rule for
// anyone being given work or ownership in a project.
func (s *Service) requireMember(ctx context.Context, projectID, userID int) error {
	if u, err := s.Repo.GetUserByID(ctx, userID); err == nil && u.DisabledAt != nil {
		return authz.BadRequest("user %d (%s) is deactivated and can't be given work", userID, u.Email)
	}
	a, err := s.Repo.GetProjectAccess(ctx, userID, projectID)
	if err != nil {
		return err
	}
	if !a.Member && a.OrgRole != domain.OrgRoleOwner && a.OrgRole != domain.OrgRoleAdmin {
		return authz.BadRequest("user %d has no access to project %d; add them to one of its teams first", userID, projectID)
	}
	return nil
}

// ---- Tickets ----

// LoadTask fetches a ticket and checks that p may perform action on it.
func (s *Service) LoadTask(ctx context.Context, p authz.Principal, taskID int, action authz.TaskAction) (*domain.Task, authz.Level, error) {
	t, err := s.Repo.GetTask(ctx, taskID)
	if err != nil {
		return nil, authz.None, authz.NotFound("task %d not found", taskID)
	}
	lvl, err := s.Authz.ProjectLevel(ctx, p, t.ProjectID)
	if err != nil {
		return nil, authz.None, err
	}
	if err := authz.CheckTask(lvl, t, p.UserID, action); err != nil {
		return nil, lvl, err
	}
	return t, lvl, nil
}

// ResolveNewTaskOwner picks the owner of a ticket being created: the creator
// by default, nobody for "none"/"unassigned", or the referenced project member.
func (s *Service) ResolveNewTaskOwner(ctx context.Context, p authz.Principal, projectID int, ref string) (*int, error) {
	switch strings.ToLower(strings.TrimSpace(ref)) {
	case "":
		return p.UserRef(), nil
	case "none", "unassigned":
		return nil, nil
	}
	id, err := s.Authz.ResolveUser(ctx, p, ref)
	if err != nil {
		return nil, err
	}
	if err := s.requireMember(ctx, projectID, id); err != nil {
		return nil, err
	}
	return &id, nil
}

// AssignTask gives a ticket to the referenced user, claims it ("me"), or
// releases it ("none"). The change is a compare-and-set in the TaskActor
// against the owner checked here, so concurrent claims can't both win.
func (s *Service) AssignTask(ctx context.Context, p authz.Principal, taskID int, ref string) (*domain.Task, error) {
	var newOwner *int
	action := authz.TaskUnassign
	switch strings.ToLower(strings.TrimSpace(ref)) {
	case "":
		return nil, authz.BadRequest(`user is required: a user id or email, "me" to claim, or "none" to release`)
	case "none", "unassigned":
	default:
		id, err := s.Authz.ResolveUser(ctx, p, ref)
		if err != nil {
			return nil, err
		}
		newOwner = &id
		action = authz.TaskAssign
		if id == p.UserID {
			action = authz.TaskClaim
		}
	}

	t, lvl, err := s.LoadTask(ctx, p, taskID, action)
	requireIdle := false
	if err != nil && action == authz.TaskClaim {
		// Taking over someone else's ticket is a reassignment.
		action = authz.TaskAssign
		t, lvl, err = s.LoadTask(ctx, p, taskID, action)
	}
	if err != nil {
		if t, requireIdle, err = s.openReassign(ctx, taskID, lvl, action, err); err != nil {
			return nil, err
		}
	}
	if newOwner != nil {
		if err := s.requireMember(ctx, t.ProjectID, *newOwner); err != nil {
			return nil, err
		}
	}

	return s.setOwner(ctx, p, t, newOwner, requireIdle, leadInUI(p, lvl))
}

// openReassign retries a reassignment the strict rules refused (refusal)
// under the project's open-reassign setting. When that allows it, the
// TaskActor must re-check that nobody started the ticket in the meantime.
func (s *Service) openReassign(ctx context.Context, taskID int, lvl authz.Level, action authz.TaskAction, refusal error) (*domain.Task, bool, error) {
	if lvl < authz.Member {
		return nil, false, refusal
	}
	t, err := s.Repo.GetTask(ctx, taskID)
	if err != nil {
		return nil, false, refusal
	}
	proj, err := s.Repo.GetProject(ctx, t.ProjectID)
	if err != nil || !proj.OpenReassign {
		return nil, false, refusal
	}
	if t.State == domain.TaskStateInProgress {
		return nil, false, authz.Forbidden("task %d is in progress; only its owner, a team lead, or a project owner can reassign it until it stops", taskID)
	}
	if !authz.OpenReassign(lvl, t, action) {
		return nil, false, refusal
	}
	return t, true, nil
}

// ClaimTask takes an unassigned ticket for p. Unlike AssignTask(…, "me"),
// it never takes a ticket from its current owner, whatever p's level:
// reassigning someone's work must be explicit.
func (s *Service) ClaimTask(ctx context.Context, p authz.Principal, taskID int) (*domain.Task, error) {
	if !p.HasUser() {
		return nil, authz.BadRequest("claiming needs a signed-in AMP user")
	}
	t, lvl, err := s.LoadTask(ctx, p, taskID, authz.TaskClaim)
	if err != nil {
		return nil, err
	}
	if t.OwnerUserID != nil && *t.OwnerUserID != p.UserID {
		// Even global admins, who pass every check, don't claim taken work.
		return nil, authz.Forbidden("task %d is owned by user %d; claiming only takes unassigned tickets, use assign to reassign it", t.ID, *t.OwnerUserID)
	}
	return s.setOwner(ctx, p, t, p.UserRef(), false, leadInUI(p, lvl))
}

// leadInUI reports whether an ownership change by p (at level lvl) keeps
// the ticket's plan approval: p is a project lead or above acting in the
// AMP web UI. Off the UI (MCP, an API token, an agent) a lead's change is
// treated like a Member's, because approving on someone's behalf is
// UI-only (ApproveTickets) and an agent under a lead's login must not move
// approved work to someone who never approved it (AMP #2223).
func leadInUI(p authz.Principal, lvl authz.Level) bool {
	return lvl >= authz.Lead && p.InUI()
}

// setOwner changes t's owner through its task actor. byLead (the caller is
// a project lead or above, in the web UI: leadInUI) keeps the ticket's plan approval; anyone else's
// change drops it when the ticket stops being the person it was approved
// for (AMP #2218; see domain.Task.DropsApprovalOn). Handover goes through
// AssignTask, so the same rule applies to it.
func (s *Service) setOwner(ctx context.Context, p authz.Principal, t *domain.Task, newOwner *int, requireIdle, byLead bool) (*domain.Task, error) {
	pid, err := s.Registry.Get(t.ProjectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplyGetTask, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgAssignTask{
		TaskID:      t.ID,
		ExpectOwner: t.OwnerUserID,
		NewOwner:    newOwner,
		RequireIdle: requireIdle,
		ByLead:      byLead,
		By:          p.UserRef(),
		ByName:      p.Email,
		ReplyCh:     replyCh,
	})
	reply := actor.Await(ctx, replyCh)
	if reply.Err != nil {
		return nil, &authz.Error{Status: 409, Msg: reply.Err.Error()}
	}
	return reply.Task, nil
}

// Dispatch starts work on a ticket; an unassigned ticket is claimed by the
// dispatching user in the same step.
func (s *Service) Dispatch(ctx context.Context, p authz.Principal, taskID int, agentID string) (*domain.Task, error) {
	t, lvl, err := s.LoadTask(ctx, p, taskID, authz.TaskDispatch)
	if err != nil {
		return nil, err
	}
	proj, err := s.Repo.GetProject(ctx, t.ProjectID)
	if err := planApprovalGate(proj, err, t, s.PlanURL(t.ProjectID)); err != nil {
		return nil, err
	}
	pid, err := s.Registry.Get(t.ProjectID)
	if err != nil {
		return nil, err
	}
	replyCh := make(chan actor.ReplySimple, 1)
	s.Registry.System().Root.Send(pid, &actor.MsgDispatchTask{
		TaskID: taskID, AgentID: agentID,
		By: p.UserRef(), Override: lvl >= authz.OrgAdmin,
		ReplyCh: replyCh,
	})
	if reply := actor.Await(ctx, replyCh); reply.Err != nil {
		return nil, reply.Err
	}
	return t, nil
}

// planApprovalGate decides whether t may be dispatched as far as plan
// approval goes: with the project's setting on, a ticket a person hasn't
// approved since it was last planned (Task.Approved, per ticket since AMP
// #2198) waits for its owner, or a lead on their behalf. It fails closed: if
// the project couldn't be loaded (lookupErr), we can't tell whether the gate
// applies, so the dispatch is refused with a retryable 503 rather than let
// through unchecked.
func planApprovalGate(proj *domain.Project, lookupErr error, t *domain.Task, planURL string) error {
	if lookupErr != nil || proj == nil {
		reason := "project not found"
		if lookupErr != nil {
			reason = lookupErr.Error()
		}
		return &authz.Error{Status: 503, Msg: fmt.Sprintf("ticket %d not dispatched: couldn't load project %d to check plan approval (%s); "+
			"nothing was started, try the dispatch again", t.ID, t.ProjectID, reason)}
	}
	if proj.RequirePlanApproval && proj.AwaitsApproval(t) {
		return authz.Forbidden("ticket %d isn't approved: nobody has approved it since it was planned or its scope last changed, and "+
			"this project needs each ticket approved before agents start it. Its owner approves it in the AMP web UI (Approve plan; "+
			"a project lead may approve it on their behalf): %s. "+
			"Agents can't approve: stop and ask your person to approve it there.", t.ID, planURL)
	}
	return nil
}

// ---- Epics and stories ----

// UpdateProject changes a project's name, description, or settings; it
// needs project owner access. Callers publish project.updated.
func (s *Service) UpdateProject(ctx context.Context, p authz.Principal, projectID int, u domain.ProjectSettings) (*domain.Project, error) {
	if u.Name == nil && u.Description == nil && u.OpenReassign == nil && u.RequirePlanApproval == nil && u.Metadata == nil {
		return nil, authz.BadRequest("nothing to change: pass name, description, open_reassign, require_plan_approval, or metadata")
	}
	if _, _, err := u.Metadata.Split(); err != nil {
		return nil, authz.BadRequest("%s", err.Error())
	}
	if u.Name != nil {
		name := strings.TrimSpace(*u.Name)
		if name == "" {
			return nil, authz.BadRequest("a project needs a name")
		}
		u.Name = &name
	}
	if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Owner); err != nil {
		return nil, err
	}
	// Turning plan approval off lets agents start work nobody approved, so
	// only a person in the web UI can. Turning it on is fine from anywhere.
	if u.RequirePlanApproval != nil && !*u.RequirePlanApproval {
		if err := authz.RequireUI(p, "Turning plan approval off", s.SettingsURL(projectID)); err != nil {
			return nil, err
		}
	}
	proj, err := s.Repo.UpdateProject(ctx, projectID, u)
	if err == nil {
		s.audit(ctx, p, domain.AuditEntry{Action: "project.settings", ProjectID: intRef(projectID), TargetType: "project", TargetID: idStr(projectID),
			Summary: "changed project settings", Details: map[string]any{"name": u.Name, "description_changed": u.Description != nil,
				"open_reassign": u.OpenReassign, "require_plan_approval": u.RequirePlanApproval, "metadata": u.Metadata, "channel": p.Channel}})
	}
	return proj, err
}

// RequireEditEpic loads an epic and checks p may change it.
func (s *Service) RequireEditEpic(ctx context.Context, p authz.Principal, epicID int) (*domain.Epic, error) {
	e, err := s.Repo.GetEpic(ctx, epicID)
	if err != nil {
		return nil, authz.NotFound("epic %d not found", epicID)
	}
	lvl, err := s.Authz.RequireProject(ctx, p, e.ProjectID, authz.Member)
	if err != nil {
		return nil, err
	}
	if !authz.CanEditGrouping(lvl, e.OwnerIDs, p.UserID) {
		return nil, authz.Forbidden("epic %d: only its owners or a project owner can change it", epicID)
	}
	return e, nil
}

// RequireEditStory loads a story and checks p may change it.
func (s *Service) RequireEditStory(ctx context.Context, p authz.Principal, storyID int) (*domain.Story, error) {
	st, err := s.Repo.GetStory(ctx, storyID)
	if err != nil {
		return nil, authz.NotFound("story %d not found", storyID)
	}
	lvl, err := s.Authz.RequireProject(ctx, p, st.ProjectID, authz.Member)
	if err != nil {
		return nil, err
	}
	if !authz.CanEditGrouping(lvl, st.OwnerIDs, p.UserID) {
		return nil, authz.Forbidden("story %d: only its owners or a project owner can change it", storyID)
	}
	return st, nil
}

// ResolveOwnerRefs turns user references into IDs of project members.
func (s *Service) ResolveOwnerRefs(ctx context.Context, p authz.Principal, projectID int, refs []string) ([]int, error) {
	ids := make([]int, 0, len(refs))
	for _, ref := range refs {
		id, err := s.Authz.ResolveUser(ctx, p, ref)
		if err != nil {
			return nil, err
		}
		if err := s.requireMember(ctx, projectID, id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// ---- Live events ----

// EventFilter returns the SSE filter for p: events only for projects p can
// see. The visible set is cached per connection and refreshed every 15s, or
// at most every 2s when an event arrives for an unknown project (e.g. one
// just created or newly linked to one of p's teams).
func (s *Service) EventFilter(p authz.Principal) hub.Allow {
	if p.Super {
		return nil
	}
	var (
		mu        sync.Mutex
		visible   map[int]bool
		refreshed time.Time
	)
	refresh := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if v, err := s.Authz.VisibleProjects(ctx, p); err == nil {
			visible, refreshed = v, time.Now()
		}
	}
	return func(projectID int) bool {
		mu.Lock()
		defer mu.Unlock()
		age := time.Since(refreshed)
		if visible == nil || age > 15*time.Second || (!visible[projectID] && age > 2*time.Second) {
			refresh()
		}
		return visible[projectID]
	}
}

// FindByMetadata finds items of a kind (project, epic, story, task) whose
// effective metadata matches every key/value given, in one project or in
// every project p can see.
func (s *Service) FindByMetadata(ctx context.Context, p authz.Principal, kind string, match map[string]any, projectID, limit int) ([]repository.MetadataMatch, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if len(match) == 0 {
		return nil, authz.BadRequest("match is required: an object of key/value pairs")
	}
	clean, err := domain.Metadata(match).Clean()
	if err != nil {
		return nil, authz.BadRequest("%s", err.Error())
	}
	if len(clean) != len(match) {
		return nil, authz.BadRequest("match values can't be null")
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	var projects []int
	if projectID > 0 {
		if _, err := s.Authz.RequireProject(ctx, p, projectID, authz.Member); err != nil {
			return nil, err
		}
		projects = []int{projectID}
	} else {
		visible, err := s.Authz.VisibleProjects(ctx, p)
		if err != nil {
			return nil, err
		}
		if visible != nil {
			projects = make([]int, 0, len(visible))
			for id := range visible {
				projects = append(projects, id)
			}
		}
	}
	found, err := s.Repo.FindByMetadata(ctx, kind, clean, projects, limit)
	if err != nil && strings.HasPrefix(err.Error(), "kind must be") {
		return nil, authz.BadRequest("%s", err.Error())
	}
	return found, err
}
