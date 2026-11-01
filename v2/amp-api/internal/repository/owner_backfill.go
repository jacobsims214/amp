package repository

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/simstech/amp-api/internal/domain"
)

// ---- Owner backfill (KB decisions/human-work-ux.md §1, #2476) ----
//
// "Mine" means owner_user_id = me and nothing else. Tickets a person created
// without an owner used to count as theirs on the Board; this one-off sets
// owner_user_id = created_by on those tickets so they stay theirs.

// OwnerBackfillProject is one project's tickets that the backfill sets (or
// would set) an owner on.
type OwnerBackfillProject struct {
	ProjectID   int    `json:"project_id"`
	ProjectName string `json:"project_name"`
	OrgID       *int   `json:"org_id,omitempty"`
	TaskIDs     []int  `json:"task_ids"`
}

// ownerBackfillWhere picks the tickets: open (not completed), no owner, and
// created by an active person (users.kind = 'human', not deactivated), never
// by an agent account. $1 is a project id, or 0 for every project.
const ownerBackfillWhere = `
	t.owner_user_id IS NULL
	AND t.state <> 'completed'
	AND t.created_by = u.id
	AND u.kind = 'human'
	AND u.disabled_at IS NULL
	AND ($1::int = 0 OR t.project_id = $1::int)`

// OwnerBackfillCandidates lists, per project, the tickets the backfill would
// change. It reads only.
func (r *Repo) OwnerBackfillCandidates(ctx context.Context, projectID int) ([]OwnerBackfillProject, error) {
	rows, err := r.db.Query(ctx, `
		SELECT t.id, t.project_id FROM tasks t, users u
		WHERE `+ownerBackfillWhere+`
		ORDER BY t.project_id, t.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("owner backfill candidates: %w", err)
	}
	ids, err := scanTaskProjectPairs(rows)
	if err != nil {
		return nil, err
	}
	return ownerBackfillGroups(ctx, r.db, ids)
}

// ApplyOwnerBackfill sets owner_user_id = created_by on the candidate tickets
// and writes one audit entry per project it changed, all in one transaction:
// either every ticket and its audit entry commit, or nothing does. actor is
// the audit entry's template (who ran it, from where); the action, target,
// project and details are filled in here. Running it again changes nothing.
func (r *Repo) ApplyOwnerBackfill(ctx context.Context, projectID int, actor domain.AuditEntry) ([]OwnerBackfillProject, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `
		UPDATE tasks t SET owner_user_id = t.created_by
		FROM users u
		WHERE `+ownerBackfillWhere+`
		RETURNING t.id, t.project_id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("owner backfill: %w", err)
	}
	ids, err := scanTaskProjectPairs(rows)
	if err != nil {
		return nil, err
	}
	groups, err := ownerBackfillGroups(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		e := actor
		pid := g.ProjectID
		e.ProjectID = &pid
		e.OrgID = g.OrgID
		e.Action = "task.owner.backfill"
		e.TargetType = "project"
		e.TargetID = strconv.Itoa(g.ProjectID)
		e.TargetName = g.ProjectName
		e.Summary = fmt.Sprintf("Set the owner to the creator on %d open, unowned ticket(s) in %s", len(g.TaskIDs), g.ProjectName)
		e.Details = map[string]any{"task_ids": g.TaskIDs, "count": len(g.TaskIDs), "rule": "owner_user_id = created_by where open, unowned and created by a person"}
		if _, err := insertAudit(ctx, tx, e, false); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("owner backfill commit: %w", err)
	}
	return groups, nil
}

type taskProject struct{ task, project int }

func scanTaskProjectPairs(rows pgx.Rows) ([]taskProject, error) {
	defer rows.Close()
	var out []taskProject
	for rows.Next() {
		var p taskProject
		if err := rows.Scan(&p.task, &p.project); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type querier interface {
	queryRower
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ownerBackfillGroups groups ticket ids by project (ordered by project id,
// then ticket id) and names each project.
func ownerBackfillGroups(ctx context.Context, q querier, ids []taskProject) ([]OwnerBackfillProject, error) {
	var out []OwnerBackfillProject
	idx := map[int]int{}
	for _, p := range ids {
		i, ok := idx[p.project]
		if !ok {
			i = len(out)
			idx[p.project] = i
			out = append(out, OwnerBackfillProject{ProjectID: p.project})
		}
		out[i].TaskIDs = append(out[i].TaskIDs, p.task)
	}
	for i := range out {
		if err := q.QueryRow(ctx, `SELECT name, org_id FROM projects WHERE id = $1`, out[i].ProjectID).
			Scan(&out[i].ProjectName, &out[i].OrgID); err != nil {
			return nil, fmt.Errorf("owner backfill: project %d: %w", out[i].ProjectID, err)
		}
	}
	// Report in project order even when the UPDATE returned rows unordered.
	sortBackfill(out)
	return out, nil
}

func sortBackfill(g []OwnerBackfillProject) {
	slices.SortFunc(g, func(a, b OwnerBackfillProject) int { return a.ProjectID - b.ProjectID })
	for _, p := range g {
		slices.Sort(p.TaskIDs)
	}
}
