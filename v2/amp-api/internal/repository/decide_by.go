package repository

import (
	"context"
	"time"
)

// decisionActionable is the SQL condition (on tasks t joined to projects p)
// for a person's ticket the escalation may act on: open, and one its person
// can act on now, as My Work's Do list has it. A ticket still blocked (on
// dependencies or a schedule), waiting on plan approval, or with a decision
// being recorded right now (Record decision's claim, DecisionClaimTTL) is
// left alone: it isn't waiting on its person yet, or they are on it. Its
// date still stands, so it escalates once it is actionable again.
const decisionActionable = `t.performer = 'human' AND t.state IN ('backlog', 'in_progress')
	AND NOT (p.require_plan_approval AND t.state = 'backlog'
	         AND (t.approved_at IS NULL OR t.approved_at < COALESCE(t.planned_at, t.created_at)))
	AND NOT EXISTS (SELECT 1 FROM decision_docs d WHERE d.task_id = t.id AND d.decided_at IS NULL
	                AND d.recording_started_at > NOW() - INTERVAL '` + DecisionClaimTTL + `')`

// DueDecision is a person's open, actionable ticket whose decide-by date passed longer
// ago than its project's escalation delay, and that hasn't been escalated
// for that date (#146).
type DueDecision struct {
	TaskID    int
	ProjectID int
	DecideBy  time.Time
	Minutes   int // the project's decision_escalation_minutes
}

// DueDecisions lists the tickets the escalation sweep should act on, across
// active projects that have escalation on, oldest decide-by first.
func (r *Repo) DueDecisions(ctx context.Context) ([]DueDecision, error) {
	rows, err := r.db.Query(ctx, `
		SELECT t.id, t.project_id, t.decide_by, p.decision_escalation_minutes
		  FROM tasks t JOIN projects p ON p.id = t.project_id
		 WHERE p.decision_escalation_minutes > 0 AND p.state <> 'archived'
		   AND `+decisionActionable+`
		   AND t.decide_by IS NOT NULL AND t.decide_by_escalated_at IS NULL
		   AND t.decide_by + make_interval(mins => p.decision_escalation_minutes) <= NOW()
		 ORDER BY t.decide_by, t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueDecision
	for rows.Next() {
		var d DueDecision
		if err := rows.Scan(&d.TaskID, &d.ProjectID, &d.DecideBy, &d.Minutes); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ClaimDecisionEscalation marks the ticket escalated for decideBy, the date
// the sweep read. It reports false (and changes nothing) when someone got
// there first: the date changed or was cleared since, the ticket was done
// or stopped being actionable (decisionActionable), or another pass already
// escalated it. That is what makes escalation
// happen once per ticket per overdue date.
func (r *Repo) ClaimDecisionEscalation(ctx context.Context, taskID int, decideBy time.Time) (bool, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE tasks t SET decide_by_escalated_at = NOW()
		  FROM projects p
		 WHERE p.id = t.project_id AND t.id = $1 AND t.decide_by = $2 AND t.decide_by_escalated_at IS NULL
		   AND `+decisionActionable, taskID, decideBy)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
