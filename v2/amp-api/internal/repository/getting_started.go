package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// NoteAgentContact records that principal ('user:<id>' or 'local-dev')
// reached AMP over MCP through client (mcp, harness). The first contact and
// its client are kept; later ones refresh last_at at most once a minute
// (migration 050).
func (r *Repo) NoteAgentContact(ctx context.Context, principal string, userID, managedBy *int, client string) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO agent_contacts (principal, user_id, managed_by, client) VALUES ($1, $2, $3, $4)
		ON CONFLICT (principal) DO UPDATE
		   SET last_at = NOW(), managed_by = EXCLUDED.managed_by
		 WHERE agent_contacts.last_at < NOW() - interval '1 minute'`,
		principal, userID, managedBy, client)
	return err
}

// AgentContact is the earliest sign that an agent reached AMP as someone.
type AgentContact struct {
	At     time.Time
	Client string // mcp, harness, or token (an API token was used)
}

// FirstAgentContact returns the earliest agent contact for principal, or
// for userID (0: none) through an agent account they manage or an API token
// of theirs or of those accounts. nil when there's none.
func (r *Repo) FirstAgentContact(ctx context.Context, principal string, userID int) (*AgentContact, error) {
	var c AgentContact
	err := r.db.QueryRow(ctx, `
		SELECT at, client FROM (
		  SELECT first_at AS at, client FROM agent_contacts
		   WHERE principal = $1 OR ($2 <> 0 AND (user_id = $2 OR managed_by = $2))
		  UNION ALL
		  SELECT t.last_used_at, 'token' FROM api_tokens t JOIN users u ON u.id = t.user_id
		   WHERE $2 <> 0 AND t.last_used_at IS NOT NULL AND (t.user_id = $2 OR u.managed_by = $2)
		) c ORDER BY at LIMIT 1`, principal, userID).Scan(&c.At, &c.Client)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// VisibleReceipt is the first receipt someone can see: the ticket and
// project it belongs to, and when it was finalized.
type VisibleReceipt struct {
	TaskID    int
	ProjectID int
	At        time.Time
}

// FirstVisibleReceipt reads the caller's first receipt in a project among
// ids (nil: every project, for a global admin), or nil when there's none.
// The visibility rule is the ones the shipped receipt views use (a receipt
// shows to whoever can read its ticket: service.GetTaskReceipt), and only a
// final receipt is ever shown, so drafts don't count. The ticket join leaves
// out a purged ticket's receipt, which has no drawer to open. One query over
// the caller's whole scope, not a per-project fan-out.
func (r *Repo) FirstVisibleReceipt(ctx context.Context, ids []int) (*VisibleReceipt, error) {
	var out VisibleReceipt
	err := r.db.QueryRow(ctx, `
		SELECT rc.task_id, rc.project_id, COALESCE(rc.finalized_at, rc.created_at)
		  FROM task_receipts rc JOIN tasks t ON t.id = rc.task_id
		 WHERE rc.state = 'final' AND ($1::int[] IS NULL OR rc.project_id = ANY($1))
		 ORDER BY rc.id LIMIT 1`, ids).Scan(&out.TaskID, &out.ProjectID, &out.At)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ProjectStart is what the checklist needs from the projects someone can see.
type ProjectStart struct {
	FirstProjectID int        // the oldest active project, 0 when there's none
	CreatedAt      *time.Time // when it was created
	PlanProjectID  int        // the project of the first plan approval, 0 when none
	PlanApprovedAt *time.Time
}

// ProjectStartFor reads the oldest active project among ids (nil: every
// project, for a global admin) and the first ticket approval that counts
// for approverID, always in an active project among ids: one they made, or
// with no AMP user (local dev, 0) one made by nobody's account. Approvals
// another person made don't check this person's step.
func (r *Repo) ProjectStartFor(ctx context.Context, ids []int, approverID int) (*ProjectStart, error) {
	var out ProjectStart
	err := r.db.QueryRow(ctx, `
		SELECT id, created_at FROM projects
		 WHERE state = 'active' AND ($1::int[] IS NULL OR id = ANY($1))
		 ORDER BY created_at, id LIMIT 1`, ids).Scan(&out.FirstProjectID, &out.CreatedAt)
	if err != nil && err != pgx.ErrNoRows {
		return nil, err
	}
	err = r.db.QueryRow(ctx, `
		SELECT t.project_id, t.approved_at FROM tasks t JOIN projects p ON p.id = t.project_id
		 WHERE t.approved_at IS NOT NULL AND p.state = 'active' AND ($1::int[] IS NULL OR p.id = ANY($1))
		   AND (CASE WHEN $2 <> 0 THEN t.approved_by = $2 ELSE t.approved_by IS NULL END)
		 ORDER BY t.approved_at, t.id LIMIT 1`, ids, approverID).Scan(&out.PlanProjectID, &out.PlanApprovedAt)
	if err != nil && err != pgx.ErrNoRows {
		return nil, err
	}
	return &out, nil
}
