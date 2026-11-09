-- Decide-by dates and escalation for a person's tickets (#146; follows the
-- decision docs of #125, docs/specs/human-decision-docs.md).
--
-- Everything here is additive: new columns are nullable or defaulted, and the
-- escalation is off (0) on every project, so nothing changes until an owner
-- turns it on.
--
-- tasks.decide_by               when a person's ticket should be decided by (a
--                               person sets it, or an agent suggests it). NULL:
--                               no date. Only human tickets carry one.
-- tasks.decide_by_escalated_at  when the escalation sweep acted on the current
--                               decide_by. Setting or clearing decide_by resets
--                               it, so a ticket escalates once per overdue
--                               transition.
--
-- projects.decision_escalation_minutes        escalate this long after decide_by
--                                             passes; 0 (default) is off.
-- projects.decision_escalation_raise_priority the escalation also raises the
--                                             ticket's priority one step.
-- projects.decision_backup_decider            the escalation hands the ticket to
--                                             this person (NULL: nobody).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS decide_by              TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS decide_by_escalated_at TIMESTAMPTZ;

ALTER TABLE projects ADD COLUMN IF NOT EXISTS decision_escalation_minutes        INT  NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS decision_escalation_raise_priority BOOL NOT NULL DEFAULT false;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS decision_backup_decider            INT  REFERENCES users(id) ON DELETE SET NULL;

-- The sweep reads open human tickets with a decide_by it hasn't acted on.
CREATE INDEX IF NOT EXISTS idx_tasks_decide_by_due ON tasks(project_id, decide_by)
 WHERE decide_by IS NOT NULL AND decide_by_escalated_at IS NULL AND state <> 'completed';
