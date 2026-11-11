// Package authz decides who can see and change what in AMP.
//
// Model (see docs/rbac-design.md):
//
//	Org ──< Team ──< team_members >── User
//	          └──< project_teams >── Project ── Epic/Story (many owners) ── Task (0..1 owner)
//
// A user's Level on a project is the highest of: global admin (Super), owner
// or admin of the project's org (OrgAdmin), member of a team linked to the
// project as owner (Owner), lead of any linked team (Lead), member of any
// linked team (Member). Everything else is None, and the project is hidden.
//
// Every REST handler and MCP tool resolves a Principal from the request and
// asks a Checker before acting. The actors below stay unaware of
// permissions, except that ticket ownership changes are compare-and-set in
// the TaskActor so concurrent claims can't both win.
package authz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/simstech/amp-api/internal/auth"
	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
	"github.com/simstech/amp-api/internal/reqctx"
)

// ---- Errors ----

// Error is an authorization failure carrying the HTTP status to report.
type Error struct {
	Status int
	Msg    string
	// Code is a stable machine-readable code, sent as the "code" member next
	// to "error" when set (the remote control API's codes, protocol 9.1).
	Code string
}

// Coded is an Error with a stable code.
func Coded(status int, code, format string, a ...any) error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, a...), Code: code}
}

// CodeOf returns err's stable code, "" when it has none.
func CodeOf(err error) string {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func (e *Error) Error() string { return e.Msg }

func Forbidden(format string, a ...any) error {
	return &Error{Status: 403, Msg: fmt.Sprintf(format, a...)}
}

func BadRequest(format string, a ...any) error {
	return &Error{Status: 400, Msg: fmt.Sprintf(format, a...)}
}

func NotFound(format string, a ...any) error {
	return &Error{Status: 404, Msg: fmt.Sprintf(format, a...)}
}

// StatusOf maps an error to an HTTP status: authz errors carry their own,
// repository not-found/conflict map to 404/409, anything else is fallback.
func StatusOf(err error, fallback int) int {
	var ae *Error
	switch {
	case errors.As(err, &ae):
		return ae.Status
	case errors.Is(err, repository.ErrNotFound):
		return 404
	case errors.Is(err, repository.ErrConflict):
		return 409
	}
	return fallback
}

// ---- Principal ----

// Principal is who is making a request.
type Principal struct {
	UserID int // 0 for the local-dev user (auth disabled) until it's provisioned
	Email  string
	Super  bool // global admin, or local dev
	// Agent is set for agent accounts (see domain.UserKindAgent).
	Agent bool
	// ViaToken is set when the request authenticated with an API token.
	ViaToken bool
	// Scope and ScopeTeamID are the API token's scope (domain.TokenScopeFull
	// or domain.TokenScopeSubmit) and, for a submit-only token, the one team
	// it may send requests to (AMP #3772). A principal that did not arrive
	// on an API token, and every full-scope token, is TokenScopeFull with no
	// team — unchanged behaviour. Scope narrows the account's authority and
	// never widens it: see AllowsSubmitScope and the guards in
	// authz/token_scope.go.
	Scope       string
	ScopeTeamID int
	// Channel is how the request reached AMP: auth.ChannelUI for a person in
	// the web UI, otherwise mcp, token, rest-other, or empty (in-process).
	// See RequireUI.
	Channel string

	// For the audit log: how the request authenticated (the auth.Auth*
	// constants; empty for in-process callers such as background jobs),
	// the API token's id, the Dex session id, and for an agent account the
	// person who manages it.
	AuthMethod string
	TokenID    int64
	SessionID  string
	ManagedBy  *int
	// Req is what the listener learned about the request (client IP, user
	// agent, request id); nil for in-process callers. It rides on the
	// Principal so it reaches the audit writer even where the request's
	// context doesn't.
	Req *reqctx.Info
}

// InUI reports whether p is a person acting in AMP's web UI: the only
// caller that may approve a plan, approve an artifact, turn plan approval
// off, or mark a person's own ticket done. Agent accounts never are, and
// neither is a request that arrived on an API token (AMP #3772): a credential
// is not a person, whatever channel label it was handed, so requester outcome
// acceptance can never be a token's act. In production an API token is already
// labeled auth.ChannelToken by channelOf; this makes the property structural
// rather than a consequence of that labeling.
func (p Principal) InUI() bool {
	return p.Channel == auth.ChannelUI && !p.Agent && !p.ViaToken && !p.SubmitOnly()
}

// Label names the caller for history rows and audit details: its email,
// or "local-dev" when auth is off.
func (p Principal) Label() string {
	if p.Email != "" {
		return p.Email
	}
	return "local-dev"
}

// RequireUI refuses unless p is a person in the web UI (InUI). what says
// what was refused ("Plan approval"), link is the page where the person
// does it; the message tells an agent to stop and ask its person.
func RequireUI(p Principal, what, link string) error {
	if p.InUI() {
		return nil
	}
	return UIOnly(what, link)
}

// UIOnly is RequireUI's refusal: what is done by a person in the web UI, at link.
func UIOnly(what, link string) error {
	return Forbidden("%s is done by a person in the AMP web UI, not by an agent or over MCP: %s. "+
		"Stop and ask your person to do it there.", what, link)
}

// LocalDev reports whether p is the identity every request has when auth
// is off. Since #124 it's also a real AMP user (HasUser) once provisioned.
func (p Principal) LocalDev() bool { return p.AuthMethod == auth.AuthLocalDev }

// HasUser reports whether the principal is a real AMP user (the local-dev
// identity is one once startup has provisioned it).
func (p Principal) HasUser() bool { return p.UserID != 0 }

// UserRef returns a pointer to the user ID for attribution columns, nil for local dev.
func (p Principal) UserRef() *int {
	if p.UserID == 0 {
		return nil
	}
	id := p.UserID
	return &id
}

// FromContext resolves the Principal from the auth middleware's Identity.
// A validated token without an AMP user (no email claim) is refused: such
// a caller can't be scoped to any org or team.
func FromContext(ctx context.Context) (Principal, error) {
	ident, ok := auth.FromContext(ctx)
	if !ok {
		return Principal{}, &Error{Status: 401, Msg: "not authenticated"}
	}
	req, _ := reqctx.FromContext(ctx)
	if ident.LocalDev {
		// Auth is off: a global admin. Once startup has provisioned it
		// (auth.Verifier.ProvisionLocalDev, #124) it is also a real AMP
		// user, so everything that needs one (My work, tokens, agent
		// accounts) works; before that, or if that failed, it has none.
		p := Principal{Super: true, Email: ident.Email, Channel: ident.Channel, AuthMethod: ident.AuthMethod, Req: req}
		if ident.User != nil {
			p.UserID = ident.User.ID
		}
		return p, nil
	}
	if ident.User == nil {
		if msg := ident.ProvisionRefusal.Message(); msg != "" {
			// Sign-in worked but JIT provisioning refused it: say why and
			// who can fix it (#69).
			return Principal{}, &Error{Status: 403, Msg: msg}
		}
		return Principal{}, Forbidden("this token has no AMP user (missing email claim); sign in again")
	}
	return Principal{
		UserID:   ident.User.ID,
		Email:    ident.User.Email,
		Super:    ident.HasRole(domain.RoleAdmin) && !ident.User.IsAgent(),
		Agent:    ident.User.IsAgent(),
		ViaToken: ident.APIToken,
		// The credential's scope. Empty (a Dex session, an in-process
		// caller) is full: only an API token can carry a narrower one.
		Scope:       tokenScopeOrFull(ident.TokenScope),
		ScopeTeamID: tokenScopeTeam(ident.TokenScopeTeamID),
		Channel:     ident.Channel,

		AuthMethod: ident.AuthMethod,
		TokenID:    ident.TokenID,
		SessionID:  ident.SessionID,
		ManagedBy:  ident.User.ManagedBy,
		Req:        req,
	}, nil
}

// tokenScopeOrFull normalizes a token's scope for a Principal: anything that
// isn't the explicit submit scope is full, so a Dex session (no scope), a
// pre-scope token row and a future value all behave exactly as before. Only
// domain.TokenScopeSubmit narrows anything (AMP #3772).
func tokenScopeOrFull(scope string) string {
	if scope == domain.TokenScopeSubmit {
		return domain.TokenScopeSubmit
	}
	return domain.TokenScopeFull
}

// tokenScopeTeam is a submit scope's team as a plain int, 0 when it has none.
func tokenScopeTeam(teamID *int) int {
	if teamID == nil {
		return 0
	}
	return *teamID
}

// ---- Levels ----

// Level is a principal's effective access to one project.
type Level int

const (
	None     Level = iota
	Member         // on a team linked to the project
	Lead           // lead of a team linked to the project
	Owner          // on a team linked to the project as owner
	OrgAdmin       // owner/admin of the project's org
	Super          // global admin
)

func (l Level) String() string {
	return [...]string{"none", "member", "lead", "owner", "org_admin", "global_admin"}[l]
}

// levelFromAccess turns the repository's raw access row into a Level.
func levelFromAccess(a repository.ProjectAccess) Level {
	switch {
	case a.OrgRole == domain.OrgRoleOwner || a.OrgRole == domain.OrgRoleAdmin:
		return OrgAdmin
	case a.OwnerTeam:
		return Owner
	case a.Lead:
		return Lead
	case a.Member:
		return Member
	}
	return None
}

// LevelFromInt maps ListProjectMembers' numeric level.
func LevelFromInt(n int) Level { return Level(n) }

// ---- Checker ----

// Checker answers access questions against the database.
type Checker struct {
	repo *repository.Repo
}

func NewChecker(repo *repository.Repo) *Checker { return &Checker{repo: repo} }

// ProjectLevel returns p's level on projectID. A submit-scoped credential has
// none, on every project, whatever its account's own level is (AMP #3772).
func (c *Checker) ProjectLevel(ctx context.Context, p Principal, projectID int) (Level, error) {
	if lvl, scoped := scopeProjectLevel(p, projectID); scoped {
		return lvl, nil
	}
	if p.Super {
		return Super, nil
	}
	a, err := c.repo.GetProjectAccess(ctx, p.UserID, projectID)
	if err != nil {
		return None, err
	}
	return levelFromAccess(a), nil
}

// RequireProject fails unless p has at least min on projectID. A project p
// can't see at all is reported as not found, so its existence isn't leaked.
func (c *Checker) RequireProject(ctx context.Context, p Principal, projectID int, min Level) (Level, error) {
	lvl, err := c.ProjectLevel(ctx, p, projectID)
	if err != nil {
		return None, err
	}
	if lvl == None {
		return None, NotFound("project %d not found", projectID)
	}
	if lvl < min {
		return lvl, Forbidden("project %d: requires %s access, you have %s", projectID, min, lvl)
	}
	return lvl, nil
}

// VisibleProjects returns the IDs of projects p can see, or nil meaning all.
// A submit-scoped credential sees the empty set, never nil (AMP #3772): nil
// means every project in that method's contract, which is the opposite of what
// a narrowed credential may reach.
func (c *Checker) VisibleProjects(ctx context.Context, p Principal) (map[int]bool, error) {
	if visible, scoped := scopedVisibleProjects(p); scoped {
		return visible, nil
	}
	if p.Super {
		return nil, nil
	}
	return c.repo.AccessibleProjectIDs(ctx, p.UserID)
}

// OrgRole returns p's role in orgID ("" if not a member). A submit-scoped
// credential holds no org role, so every org-scoped capability is closed to it
// (AMP #3772).
func (c *Checker) OrgRole(ctx context.Context, p Principal, orgID int) (string, error) {
	if role, scoped := scopedOrgRole(p); scoped {
		return role, nil
	}
	if !p.HasUser() {
		return "", nil
	}
	return c.repo.GetOrgRole(ctx, orgID, p.UserID)
}

// RequireOrgMember fails unless p is in orgID (or a global admin). A
// submit-scoped credential administers no org, global admin or not (AMP #3772).
func (c *Checker) RequireOrgMember(ctx context.Context, p Principal, orgID int) (string, error) {
	if err := p.requireNotScoped(); err != nil {
		return "", err
	}
	role, err := c.OrgRole(ctx, p, orgID)
	if err != nil {
		return "", err
	}
	if role == "" && !p.Super {
		return "", NotFound("org %d not found", orgID)
	}
	return role, nil
}

// RequireOrgAdmin fails unless p is an owner/admin of orgID or a global admin.
// A submit-scoped credential is never one (AMP #3772).
func (c *Checker) RequireOrgAdmin(ctx context.Context, p Principal, orgID int) (string, error) {
	role, err := c.RequireOrgMember(ctx, p, orgID)
	if err != nil {
		return "", err
	}
	if !p.Super && role != domain.OrgRoleOwner && role != domain.OrgRoleAdmin {
		return role, Forbidden("org %d: requires org owner or admin", orgID)
	}
	return role, nil
}

// RequireTeamManager fails unless p can manage team membership: org
// owner/admin, global admin, or a lead of the team. A submit-scoped credential
// never manages a team — including the one it may submit to, because managing
// it would let the credential hand itself more (AMP #3772).
func (c *Checker) RequireTeamManager(ctx context.Context, p Principal, team *domain.Team) error {
	if err := p.requireNotScoped(); err != nil {
		return err
	}
	role, err := c.RequireOrgMember(ctx, p, team.OrgID)
	if err != nil {
		return err
	}
	if p.Super || role == domain.OrgRoleOwner || role == domain.OrgRoleAdmin {
		return nil
	}
	teamRole, err := c.repo.GetTeamRole(ctx, team.ID, p.UserID)
	if err != nil {
		return err
	}
	if teamRole != domain.TeamRoleLead {
		return Forbidden("team %d: requires team lead or org admin", team.ID)
	}
	return nil
}

// ---- Capabilities outside the project Level ladder ----

// TeamSubmit fails unless p may submit a request to teamID's inbox.
//
// Submitting is deliberately not a project Level (approved contract artifact 36
// v2 §2, build slice B3): a project Level >= Member grants TaskRead on every
// ticket, comment and history row in the project, and a requester must never
// get that. So this check is team-scoped and never consults project access —
// no Level value, not even Owner, satisfies it on its own.
//
// It passes for:
//
//   - a member of the team (team_members row, member or lead);
//   - an owner or admin of the team's org, and a global admin — the people who
//     administer the team and its submit access (RequireTeamManager, D14);
//   - the holder of an in-force submit grant on that team (team_submit_grants),
//     which is how an invited customer who is in no org and on no team submits.
//
// The grant is re-read from the database on every call and nothing here caches
// an allow-set: a revoked grant fails on the very next call with nothing to
// reset (revocation is a state on the row, `revoked_at`, not a delete — see
// repository.RevokeTeamSubmit). Tenancy comes from the rows themselves: the
// grant's composite FK binds (team_id, org_id), team_members binds the team,
// and the org role is read for the team's own org — so a grant or membership in
// another org can never satisfy a team here.
//
// Refusals follow RequireProject's shape so existence never leaks: a caller with
// no relationship to the team at all (not in its org, no grant) is told the team
// is not found, while an org member who can already see the team and simply may
// not submit to it gets 403 with the reason.
func (c *Checker) TeamSubmit(ctx context.Context, p Principal, teamID int) error {
	// A submit-scoped credential may only submit to the one team it is bound
	// to, and only while its account could submit there anyway (AMP #3772).
	if scoped, err := c.scopeTeamSubmit(ctx, p, teamID); scoped {
		return err
	}
	return c.teamSubmit(ctx, p, teamID)
}

// teamSubmit is TeamSubmit's ordinary path, for a principal that is not
// narrowed by a credential scope.
func (c *Checker) teamSubmit(ctx context.Context, p Principal, teamID int) error {
	team, err := c.repo.GetTeam(ctx, teamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return NotFound("team %d not found", teamID)
		}
		return err
	}
	if p.Super {
		return nil
	}
	if !p.HasUser() {
		// Local dev before its user row is provisioned (auth disabled): it has
		// no membership and no grant of its own, and being Super is the whole
		// of its authority — handled above.
		return NotFound("team %d not found", teamID)
	}

	// A team member or the team's lead submits to their own inbox.
	teamRole, err := c.repo.GetTeamRole(ctx, team.ID, p.UserID)
	if err != nil {
		return err
	}
	if teamRole != "" {
		return nil
	}

	// Org owner/admin administer the team (D14), so they may submit to it.
	orgRole, err := c.repo.GetOrgRole(ctx, team.OrgID, p.UserID)
	if err != nil {
		return err
	}
	if orgRole == domain.OrgRoleOwner || orgRole == domain.OrgRoleAdmin {
		return nil
	}

	// The grant: one indexed lookup, read now rather than remembered. This is
	// the line that makes revocation and re-granting take effect per request.
	granted, err := c.repo.HasTeamSubmitGrant(ctx, team.ID, p.UserID)
	if err != nil {
		return err
	}
	if granted {
		return nil
	}

	if orgRole == "" {
		// No membership, no grant, not in the org: the inbox is not theirs to
		// know about.
		return NotFound("team %d not found", teamID)
	}
	return Forbidden("team %d: submitting requires team membership or submit access", team.ID)
}

// ResolveUser turns a user reference — "me", a numeric ID, or an email —
// into a user ID.
func (c *Checker) ResolveUser(ctx context.Context, p Principal, ref string) (int, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return 0, BadRequest("user is required")
	case strings.EqualFold(ref, "me"):
		if !p.HasUser() {
			return 0, BadRequest(`"me" needs a signed-in AMP user`)
		}
		return p.UserID, nil
	}
	if id, err := strconv.Atoi(ref); err == nil {
		if _, err := c.repo.GetUserByID(ctx, id); err != nil {
			return 0, NotFound("user %d not found", id)
		}
		return id, nil
	}
	u, err := c.repo.GetUserByEmail(ctx, ref)
	if err != nil {
		return 0, NotFound("no AMP user with email %q (they must sign in once, or be invited)", ref)
	}
	return u.ID, nil
}

// ---- Decisions (pure, so they can be tested exhaustively) ----

// CanEditGrouping: epics and stories are editable by their owners and by
// anyone with Owner level or above on the project.
func CanEditGrouping(lvl Level, ownerIDs []int, me int) bool {
	return lvl >= Owner || (me != 0 && slices.Contains(ownerIDs, me))
}

// TaskAction is something a principal wants to do to a ticket.
type TaskAction int

const (
	TaskRead     TaskAction = iota // view, history, comment — anyone on the project
	TaskWork                       // update fields, complete, block, change state, schedule — owner only
	TaskDispatch                   // start work; an unassigned ticket is claimed by the dispatcher
	TaskClaim                      // take an unassigned ticket
	TaskAssign                     // give the ticket to someone
	TaskUnassign                   // release the ticket back to the pool
	TaskDelete
	// TaskPlan: change how a ticket fits the plan — its dependencies, or which
	// story it belongs to.
	TaskPlan
)

func ownedBy(t *domain.Task, me int) bool {
	return t.OwnerUserID != nil && me != 0 && *t.OwnerUserID == me
}

// CheckTask decides whether a principal at lvl may perform action on t.
// Local dev (me == 0) only reaches here as Super.
func CheckTask(lvl Level, t *domain.Task, me int, action TaskAction) error {
	if lvl == None {
		return NotFound("task %d not found", t.ID)
	}
	if lvl == Super {
		return nil
	}
	unassigned := t.OwnerUserID == nil
	mine := ownedBy(t, me)
	switch action {
	case TaskRead:
		return nil
	case TaskWork, TaskDispatch:
		// Only the owner works a ticket (unassigned ones are open to anyone
		// on the project). Leads and project owners reassign rather than
		// act under someone else's name; org admins keep an escape hatch.
		if mine || unassigned || lvl >= OrgAdmin {
			return nil
		}
	case TaskClaim:
		if unassigned || mine {
			return nil
		}
		return Forbidden("task %d is owned by user %d; ask them or a lead to reassign it", t.ID, *t.OwnerUserID)
	case TaskAssign:
		if mine || unassigned || lvl >= Lead {
			return nil
		}
	case TaskUnassign:
		if mine || unassigned || lvl >= Lead {
			return nil
		}
	case TaskPlan:
		// Whoever planned it (its creator), its owner, anyone for an
		// unassigned one, and leads and up for any.
		createdByMe := t.CreatedBy != nil && me != 0 && *t.CreatedBy == me
		if mine || unassigned || createdByMe || lvl >= Lead {
			return nil
		}
	case TaskDelete:
		createdByMe := t.CreatedBy != nil && me != 0 && *t.CreatedBy == me
		if mine || createdByMe || lvl >= Lead {
			return nil
		}
	}
	if unassigned {
		return Forbidden("task %d: only its creator, a team lead, or a project owner can do that", t.ID)
	}
	return Forbidden("task %d is owned by user %d; only its owner, a team lead, or a project owner can do that", t.ID, *t.OwnerUserID)
}

// OpenReassign reports whether a project with open reassign turned on lets
// someone at lvl reassign or release a ticket CheckTask refused: any
// project member may, as long as nobody is working the ticket. Claiming is
// not covered: a claim only ever takes unassigned work, so agents racing
// for the pool never take each other's tickets.
func OpenReassign(lvl Level, t *domain.Task, action TaskAction) bool {
	if lvl < Member || t.State == domain.TaskStateInProgress {
		return false
	}
	return action == TaskAssign || action == TaskUnassign
}

// ---- Task views (what a board or list shows by default) ----

// TaskView selects which tickets a list returns.
type TaskView struct {
	Mode   string // "mine" | "unassigned" | "any" | "user"
	UserID int    // for Mode "user"
}

// DefaultTaskView is the caller's own work.
var DefaultTaskView = TaskView{Mode: "mine"}

// ParseTaskView parses an owner filter: "" or "mine" (default), "unassigned",
// "any"/"all", "me", or a user reference resolved by the Checker.
func (c *Checker) ParseTaskView(ctx context.Context, p Principal, s string) (TaskView, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "mine":
		return DefaultTaskView, nil
	case "unassigned", "none":
		return TaskView{Mode: "unassigned"}, nil
	case "any", "all", "everyone":
		return TaskView{Mode: "any"}, nil
	}
	id, err := c.ResolveUser(ctx, p, s)
	if err != nil {
		return TaskView{}, err
	}
	return TaskView{Mode: "user", UserID: id}, nil
}

// Includes reports whether t is in the view for principal me.
//
// "mine" is: tickets I own, plus unassigned tickets I created, plus
// unassigned tickets with no creator — rows from before attribution
// existed, which would otherwise vanish from every default board.
// me == 0 (no AMP user: local dev before it's provisioned, #124) shows
// everything.
func (v TaskView) Includes(t *domain.Task, me int) bool {
	switch v.Mode {
	case "any":
		return true
	case "unassigned":
		return t.OwnerUserID == nil
	case "user":
		return t.OwnerUserID != nil && *t.OwnerUserID == v.UserID
	}
	if me == 0 {
		return true
	}
	if t.OwnerUserID != nil {
		return *t.OwnerUserID == me
	}
	return t.CreatedBy == nil || *t.CreatedBy == me
}

// FilterTasks returns the tasks in view, preserving order.
func FilterTasks(tasks []domain.Task, v TaskView, me int) []domain.Task {
	out := make([]domain.Task, 0, len(tasks))
	for i := range tasks {
		if v.Includes(&tasks[i], me) {
			out = append(out, tasks[i])
		}
	}
	return out
}
