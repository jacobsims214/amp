package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Orgs, teams, invites, and project access ----
//
// Access model: a user reaches a project through a team linked to it
// (project_teams), or as an owner/admin of the project's org. Global admins
// (user_roles 'admin') are handled above this layer, in authz.

// ErrNotFound marks lookups that matched nothing, so handlers can 404.
var ErrNotFound = errors.New("not found")

// ErrConflict marks writes rejected by a uniqueness rule or a "last owner" guard.
var ErrConflict = errors.New("conflict")

// conflictError is a user-facing message that matches ErrConflict.
type conflictError struct{ msg string }

func (e *conflictError) Error() string        { return e.msg }
func (e *conflictError) Is(target error) bool { return target == ErrConflict }

func conflictf(format string, a ...any) error {
	return &conflictError{msg: fmt.Sprintf(format, a...)}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ProjectAccess is the raw material for a user's access level on a project.
type ProjectAccess struct {
	OrgRole   string // the user's role in the project's org, "" if none
	OwnerTeam bool   // on a team linked to the project as owner
	Lead      bool   // lead of a team linked to the project
	Member    bool   // on any team linked to the project
}

// GetProjectAccess returns how userID reaches projectID.
func (r *Repo) GetProjectAccess(ctx context.Context, userID, projectID int) (ProjectAccess, error) {
	var a ProjectAccess
	err := r.db.QueryRow(ctx, `
		SELECT
		  COALESCE((SELECT om.role FROM org_members om JOIN projects p ON p.org_id = om.org_id
		            WHERE p.id = $1 AND om.user_id = $2), ''),
		  COALESCE(bool_or(pt.role = 'owner'), false),
		  COALESCE(bool_or(tm.role = 'lead'), false),
		  COUNT(pt.team_id) > 0
		FROM project_teams pt
		JOIN team_members tm ON tm.team_id = pt.team_id AND tm.user_id = $2
		WHERE pt.project_id = $1`,
		projectID, userID,
	).Scan(&a.OrgRole, &a.OwnerTeam, &a.Lead, &a.Member)
	return a, err
}

// AccessibleProjectIDs returns every project userID can see (any state).
func (r *Repo) AccessibleProjectIDs(ctx context.Context, userID int) (map[int]bool, error) {
	rows, err := r.db.Query(ctx, `
		SELECT pt.project_id FROM project_teams pt
		JOIN team_members tm ON tm.team_id = pt.team_id
		WHERE tm.user_id = $1
		UNION
		SELECT p.id FROM projects p
		JOIN org_members om ON om.org_id = p.org_id
		WHERE om.user_id = $1 AND om.role IN ('owner', 'admin')`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ListProjectMembers returns every user with access to the project and
// their effective level: 4 org admin, 3 owner team, 2 team lead, 1 member.
func (r *Repo) ListProjectMembers(ctx context.Context, projectID int) ([]struct {
	User  domain.User
	Level int
}, error) {
	rows, err := r.db.Query(ctx, `
		WITH access AS (
		  SELECT tm.user_id,
		         CASE WHEN pt.role = 'owner' THEN 3 WHEN tm.role = 'lead' THEN 2 ELSE 1 END AS lvl
		  FROM project_teams pt JOIN team_members tm ON tm.team_id = pt.team_id
		  WHERE pt.project_id = $1
		  UNION ALL
		  SELECT om.user_id, 4
		  FROM org_members om JOIN projects p ON p.org_id = om.org_id
		  WHERE p.id = $1 AND om.role IN ('owner', 'admin')
		)
		SELECT u.id, u.email, u.display_name, MAX(a.lvl), u.kind, u.disabled_at
		FROM access a JOIN users u ON u.id = a.user_id
		GROUP BY u.id, u.email, u.display_name, u.kind, u.disabled_at
		ORDER BY u.email`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		User  domain.User
		Level int
	}
	for rows.Next() {
		var m struct {
			User  domain.User
			Level int
		}
		if err := rows.Scan(&m.User.ID, &m.User.Email, &m.User.DisplayName, &m.Level, &m.User.Kind, &m.User.DisabledAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- Users lookup ----

func (r *Repo) GetUserByID(ctx context.Context, id int) (*domain.User, error) {
	u := &domain.User{}
	err := r.db.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id = $1`, id,
	).Scan(userDest(u)...)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("user %d: %w", id, ErrNotFound)
	}
	return u, err
}

// ---- Orgs ----

const orgCols = `o.id, o.name, o.slug, o.created_by, o.created_at`

func scanOrg(row rowScanner, o *domain.Org) error {
	return row.Scan(&o.ID, &o.Name, &o.Slug, &o.CreatedBy, &o.CreatedAt)
}

// CreateOrg creates an org; ownerID, when set, becomes its first owner.
func (r *Repo) CreateOrg(ctx context.Context, name, slug string, createdBy, ownerID *int) (*domain.Org, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	o := &domain.Org{}
	err = scanOrg(tx.QueryRow(ctx,
		`INSERT INTO orgs AS o (name, slug, created_by) VALUES ($1, $2, $3) RETURNING `+orgCols,
		name, slug, createdBy), o)
	if isUniqueViolation(err) {
		return nil, conflictf("org slug %q already exists", slug)
	}
	if err != nil {
		return nil, err
	}
	if ownerID != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, o.ID, *ownerID); err != nil {
			return nil, err
		}
	}
	return o, tx.Commit(ctx)
}

func (r *Repo) GetOrg(ctx context.Context, id int) (*domain.Org, error) {
	o := &domain.Org{}
	err := scanOrg(r.db.QueryRow(ctx, `SELECT `+orgCols+` FROM orgs o WHERE o.id = $1`, id), o)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("org %d: %w", id, ErrNotFound)
	}
	return o, err
}

// ListOrgs returns all orgs (userID nil) or the orgs userID belongs to,
// with the user's role in each.
func (r *Repo) ListOrgs(ctx context.Context, userID *int) ([]domain.Org, error) {
	q := `SELECT ` + orgCols + `, COALESCE(om.role, '') FROM orgs o
	      LEFT JOIN org_members om ON om.org_id = o.id AND om.user_id = $1`
	if userID != nil {
		q += ` WHERE om.user_id IS NOT NULL`
	}
	q += ` ORDER BY o.name`
	var uid any
	if userID != nil {
		uid = *userID
	}
	rows, err := r.db.Query(ctx, q, uid)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row rowScanner, o *domain.Org) error {
		return row.Scan(&o.ID, &o.Name, &o.Slug, &o.CreatedBy, &o.CreatedAt, &o.MyRole)
	})
}

func (r *Repo) RenameOrg(ctx context.Context, id int, name string) error {
	tag, err := r.db.Exec(ctx, `UPDATE orgs SET name = $1 WHERE id = $2`, name, id)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("org %d: %w", id, ErrNotFound)
	}
	return err
}

// DeleteOrg removes an org with no projects (projects.org_id is RESTRICT).
func (r *Repo) DeleteOrg(ctx context.Context, id int) error {
	_, err := r.db.Exec(ctx, `DELETE FROM orgs WHERE id = $1`, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return conflictf("this org still has projects; move or delete them first")
	}
	return err
}

func (r *Repo) GetOrgRole(ctx context.Context, orgID, userID int) (string, error) {
	var role string
	err := r.db.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, orgID, userID).Scan(&role)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return role, err
}

func (r *Repo) ListOrgMembers(ctx context.Context, orgID int) ([]domain.Member, error) {
	rows, err := r.db.Query(ctx, `
		SELECT u.id, u.email, u.display_name, om.role, u.kind, u.disabled_at IS NOT NULL
		FROM org_members om JOIN users u ON u.id = om.user_id
		WHERE om.org_id = $1 ORDER BY u.email`, orgID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanUserMember)
}

func scanMember(row rowScanner, m *domain.Member) error {
	return row.Scan(&m.UserID, &m.Email, &m.DisplayName, &m.Role)
}

// scanUserMember also reads the account's kind and whether it's deactivated.
func scanUserMember(row rowScanner, m *domain.Member) error {
	return row.Scan(&m.UserID, &m.Email, &m.DisplayName, &m.Role, &m.Kind, &m.Disabled)
}

// SetOrgMember adds a user to an org or changes their role. Demoting the
// last owner is refused.
func (r *Repo) SetOrgMember(ctx context.Context, orgID, userID int, role string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if role != domain.OrgRoleOwner {
		if err := guardLastOrgOwner(ctx, tx, orgID, userID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role`, orgID, userID, role); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RemoveOrgMember removes a user from an org and all of its teams.
func (r *Repo) RemoveOrgMember(ctx context.Context, orgID, userID int) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := guardLastOrgOwner(ctx, tx, orgID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM team_members WHERE user_id = $2 AND team_id IN (SELECT id FROM teams WHERE org_id = $1)`, orgID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM org_members WHERE org_id = $1 AND user_id = $2`, orgID, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// guardLastOrgOwner fails if userID is the org's only owner. It locks the
// org's owner rows so two concurrent demotions can't both pass.
func guardLastOrgOwner(ctx context.Context, tx pgx.Tx, orgID, userID int) error {
	rows, err := tx.Query(ctx, `SELECT user_id FROM org_members WHERE org_id = $1 AND role = 'owner' FOR UPDATE`, orgID)
	if err != nil {
		return err
	}
	var owners []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		owners = append(owners, id)
	}
	rows.Close()
	if len(owners) == 1 && owners[0] == userID {
		return conflictf("an org needs at least one owner; make someone else an owner first")
	}
	return nil
}

// ---- Teams ----

const teamCols = `t.id, t.org_id, t.name, t.created_by, t.created_at`

func scanTeam(row rowScanner, t *domain.Team) error {
	return row.Scan(&t.ID, &t.OrgID, &t.Name, &t.CreatedBy, &t.CreatedAt)
}

func (r *Repo) CreateTeam(ctx context.Context, orgID int, name string, createdBy *int) (*domain.Team, error) {
	t := &domain.Team{}
	err := scanTeam(r.db.QueryRow(ctx,
		`INSERT INTO teams AS t (org_id, name, created_by) VALUES ($1, $2, $3) RETURNING `+teamCols,
		orgID, name, createdBy), t)
	if isUniqueViolation(err) {
		return nil, conflictf("team %q already exists in this org", name)
	}
	return t, err
}

func (r *Repo) GetTeam(ctx context.Context, id int) (*domain.Team, error) {
	t := &domain.Team{}
	err := scanTeam(r.db.QueryRow(ctx, `SELECT `+teamCols+` FROM teams t WHERE t.id = $1`, id), t)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("team %d: %w", id, ErrNotFound)
	}
	return t, err
}

func (r *Repo) ListTeams(ctx context.Context, orgID int) ([]domain.Team, error) {
	rows, err := r.db.Query(ctx, `SELECT `+teamCols+` FROM teams t WHERE t.org_id = $1 ORDER BY t.name`, orgID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanTeam)
}

// ListUserTeams returns the teams userID is on within orgID, with their role.
func (r *Repo) ListUserTeams(ctx context.Context, orgID, userID int) ([]domain.Member, error) {
	rows, err := r.db.Query(ctx, `
		SELECT t.id, t.name, '', tm.role FROM teams t
		JOIN team_members tm ON tm.team_id = t.id AND tm.user_id = $2
		WHERE t.org_id = $1 ORDER BY t.name`, orgID, userID)
	if err != nil {
		return nil, err
	}
	// Reuses Member: UserID carries the team id, Email the team name.
	return collect(rows, scanMember)
}

func (r *Repo) RenameTeam(ctx context.Context, id int, name string) error {
	tag, err := r.db.Exec(ctx, `UPDATE teams SET name = $1 WHERE id = $2`, name, id)
	if isUniqueViolation(err) {
		return conflictf("team %q already exists in this org", name)
	}
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("team %d: %w", id, ErrNotFound)
	}
	return err
}

// DeleteTeam removes a team. Refused if it is the last owner team of any
// project, which would leave that project manageable only by org admins.
func (r *Repo) DeleteTeam(ctx context.Context, id int) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var orphaned []int
	rows, err := tx.Query(ctx, `
		SELECT pt.project_id FROM project_teams pt
		WHERE pt.team_id = $1 AND pt.role = 'owner'
		  AND NOT EXISTS (SELECT 1 FROM project_teams o WHERE o.project_id = pt.project_id AND o.role = 'owner' AND o.team_id <> $1)`, id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var pid int
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return err
		}
		orphaned = append(orphaned, pid)
	}
	rows.Close()
	if len(orphaned) > 0 {
		return conflictf("this team is the only owner team of projects %v; give those projects another owner team first", orphaned)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM teams WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repo) GetTeamRole(ctx context.Context, teamID, userID int) (string, error) {
	var role string
	err := r.db.QueryRow(ctx, `SELECT role FROM team_members WHERE team_id = $1 AND user_id = $2`, teamID, userID).Scan(&role)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return role, err
}

func (r *Repo) ListTeamMembers(ctx context.Context, teamID int) ([]domain.Member, error) {
	rows, err := r.db.Query(ctx, `
		SELECT u.id, u.email, u.display_name, tm.role, u.kind, u.disabled_at IS NOT NULL
		FROM team_members tm JOIN users u ON u.id = tm.user_id
		WHERE tm.team_id = $1 ORDER BY u.email`, teamID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanUserMember)
}

// SetTeamMember adds a user to a team (and to the team's org as a member,
// if they aren't in it yet) or changes their team role.
func (r *Repo) SetTeamMember(ctx context.Context, teamID, userID int, role string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `
		INSERT INTO org_members (org_id, user_id, role)
		SELECT org_id, $2, 'member' FROM teams WHERE id = $1
		ON CONFLICT DO NOTHING`, teamID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (team_id, user_id) DO UPDATE SET role = EXCLUDED.role`, teamID, userID, role); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repo) RemoveTeamMember(ctx context.Context, teamID, userID int) error {
	_, err := r.db.Exec(ctx, `DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`, teamID, userID)
	return err
}

// ---- Project ↔ team links ----

func (r *Repo) ListProjectTeams(ctx context.Context, projectID int) ([]domain.ProjectTeam, error) {
	rows, err := r.db.Query(ctx, `
		SELECT pt.project_id, pt.team_id, t.name, pt.role
		FROM project_teams pt JOIN teams t ON t.id = pt.team_id
		WHERE pt.project_id = $1 ORDER BY pt.role, t.name`, projectID)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row rowScanner, pt *domain.ProjectTeam) error {
		return row.Scan(&pt.ProjectID, &pt.TeamID, &pt.TeamName, &pt.Role)
	})
}

// SetProjectTeam links a team to a project (or changes its role). The team
// must belong to the project's org; demoting the last owner team is refused.
func (r *Repo) SetProjectTeam(ctx context.Context, projectID, teamID int, role string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var sameOrg bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM projects p JOIN teams t ON t.org_id = p.org_id WHERE p.id = $1 AND t.id = $2)`,
		projectID, teamID).Scan(&sameOrg); err != nil {
		return err
	}
	if !sameOrg {
		return conflictf("that team belongs to a different org than this project")
	}
	if role != domain.ProjectTeamOwner {
		if err := guardLastOwnerTeam(ctx, tx, projectID, teamID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (project_id, team_id) DO UPDATE SET role = EXCLUDED.role`, projectID, teamID, role); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repo) RemoveProjectTeam(ctx context.Context, projectID, teamID int) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := guardLastOwnerTeam(ctx, tx, projectID, teamID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM project_teams WHERE project_id = $1 AND team_id = $2`, projectID, teamID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func guardLastOwnerTeam(ctx context.Context, tx pgx.Tx, projectID, teamID int) error {
	rows, err := tx.Query(ctx, `SELECT team_id FROM project_teams WHERE project_id = $1 AND role = 'owner' FOR UPDATE`, projectID)
	if err != nil {
		return err
	}
	var owners []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		owners = append(owners, id)
	}
	rows.Close()
	if len(owners) == 1 && owners[0] == teamID {
		return conflictf("a project needs at least one owner team; make another team an owner first")
	}
	return nil
}

// ---- Epic / story owners ----

func (r *Repo) AddEpicOwners(ctx context.Context, epicID int, userIDs []int) error {
	return r.setOwners(ctx, `INSERT INTO epic_owners (epic_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, epicID, userIDs)
}

func (r *Repo) RemoveEpicOwners(ctx context.Context, epicID int, userIDs []int) error {
	return r.setOwners(ctx, `DELETE FROM epic_owners WHERE epic_id = $1 AND user_id = $2`, epicID, userIDs)
}

func (r *Repo) AddStoryOwners(ctx context.Context, storyID int, userIDs []int) error {
	return r.setOwners(ctx, `INSERT INTO story_owners (story_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, storyID, userIDs)
}

func (r *Repo) RemoveStoryOwners(ctx context.Context, storyID int, userIDs []int) error {
	return r.setOwners(ctx, `DELETE FROM story_owners WHERE story_id = $1 AND user_id = $2`, storyID, userIDs)
}

func (r *Repo) setOwners(ctx context.Context, stmt string, id int, userIDs []int) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, uid := range userIDs {
		if _, err := tx.Exec(ctx, stmt, id, uid); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ---- Invites ----

const inviteCols = `id, email, org_id, org_role, team_id, team_role, invited_by, created_at, accepted_at`

func scanInvite(row rowScanner, i *domain.Invite) error {
	return row.Scan(&i.ID, &i.Email, &i.OrgID, &i.OrgRole, &i.TeamID, &i.TeamRole, &i.InvitedBy, &i.CreatedAt, &i.AcceptedAt)
}

// CreateInvite records a pending membership for an email that has never
// signed in. Re-inviting the same email to the same org/team updates the roles.
func (r *Repo) CreateInvite(ctx context.Context, inv domain.Invite) (*domain.Invite, error) {
	out := &domain.Invite{}
	err := scanInvite(r.db.QueryRow(ctx, `
		INSERT INTO invites (email, org_id, org_role, team_id, team_role, invited_by)
		VALUES (lower(trim($1)), $2, $3, $4, $5, $6)
		ON CONFLICT (lower(email), org_id, COALESCE(team_id, 0)) WHERE accepted_at IS NULL
		DO UPDATE SET org_role = EXCLUDED.org_role, team_role = EXCLUDED.team_role, invited_by = EXCLUDED.invited_by
		RETURNING `+inviteCols,
		inv.Email, inv.OrgID, inv.OrgRole, inv.TeamID, inv.TeamRole, inv.InvitedBy), out)
	return out, err
}

func (r *Repo) GetInvite(ctx context.Context, id int) (*domain.Invite, error) {
	out := &domain.Invite{}
	err := scanInvite(r.db.QueryRow(ctx, `SELECT `+inviteCols+` FROM invites WHERE id = $1`, id), out)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("invite %d: %w", id, ErrNotFound)
	}
	return out, err
}

func (r *Repo) ListPendingInvites(ctx context.Context, orgID int) ([]domain.Invite, error) {
	rows, err := r.db.Query(ctx, `SELECT `+inviteCols+` FROM invites WHERE org_id = $1 AND accepted_at IS NULL ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanInvite)
}

func (r *Repo) DeleteInvite(ctx context.Context, id int) error {
	_, err := r.db.Exec(ctx, `DELETE FROM invites WHERE id = $1 AND accepted_at IS NULL`, id)
	return err
}

// AcceptInvites applies every pending invite for the user's email: org
// membership (never downgrading an existing role) and team membership.
// Called on every sign-in; a no-op once nothing is pending.
func (r *Repo) AcceptInvites(ctx context.Context, userID int, email string) (int, error) {
	accepted, err := r.AcceptInvitesList(ctx, userID, email)
	return len(accepted), err
}

// AcceptInvitesList is AcceptInvites returning the invites it accepted
// (each grants its org role, and its team role when it names a team).
func (r *Repo) AcceptInvitesList(ctx context.Context, userID int, email string) ([]domain.Invite, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `SELECT `+inviteCols+` FROM invites WHERE lower(email) = lower($1) AND accepted_at IS NULL FOR UPDATE`, email)
	if err != nil {
		return nil, err
	}
	pending, err := collect(rows, scanInvite)
	if err != nil {
		return nil, err
	}
	for _, inv := range pending {
		if _, err := tx.Exec(ctx, `
			INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3)
			ON CONFLICT (org_id, user_id) DO UPDATE SET role = CASE
			  WHEN org_members.role = 'owner' OR EXCLUDED.role = 'owner' THEN 'owner'
			  WHEN org_members.role = 'admin' OR EXCLUDED.role = 'admin' THEN 'admin'
			  ELSE 'member' END`, inv.OrgID, userID, inv.OrgRole); err != nil {
			return nil, err
		}
		if inv.TeamID != nil {
			if _, err := tx.Exec(ctx, `
				INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, $3)
				ON CONFLICT (team_id, user_id) DO UPDATE SET role = CASE
				  WHEN team_members.role = 'lead' OR EXCLUDED.role = 'lead' THEN 'lead' ELSE 'member' END`,
				*inv.TeamID, userID, inv.TeamRole); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE invites SET accepted_at = NOW(), accepted_user_id = $2 WHERE id = $1`, inv.ID, userID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return pending, nil
}

// MoveProjectToOrg sets a project's org. Team links to teams outside the new
// org are removed (teams only reach projects in their own org), and
// ownerTeamID, when non-zero, is linked as an owner team; it must be in the
// new org. It returns the removed team links.
func (r *Repo) MoveProjectToOrg(ctx context.Context, projectID, orgID, ownerTeamID int) ([]domain.ProjectTeam, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if ownerTeamID != 0 {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM teams WHERE id = $1 AND org_id = $2)`, ownerTeamID, orgID).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, conflictf("the owner team must be a team in the new org")
		}
	}
	rows, err := tx.Query(ctx, `
		DELETE FROM project_teams pt USING teams t
		WHERE pt.project_id = $1 AND t.id = pt.team_id AND t.org_id IS DISTINCT FROM $2
		RETURNING pt.project_id, pt.team_id, t.name, pt.role`, projectID, orgID)
	if err != nil {
		return nil, err
	}
	removed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.ProjectTeam, error) {
		var pt domain.ProjectTeam
		err := row.Scan(&pt.ProjectID, &pt.TeamID, &pt.TeamName, &pt.Role)
		return pt, err
	})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE projects SET org_id = $2, updated_at = NOW() WHERE id = $1`, projectID, orgID); err != nil {
		return nil, err
	}
	if ownerTeamID != 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_teams (project_id, team_id, role) VALUES ($1, $2, 'owner')
			ON CONFLICT (project_id, team_id) DO UPDATE SET role = 'owner'`, projectID, ownerTeamID); err != nil {
			return nil, err
		}
	}
	return removed, tx.Commit(ctx)
}
