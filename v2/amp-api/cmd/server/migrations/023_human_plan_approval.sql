-- Human-only plan approval (AMP epic 162, ticket #2167).
--
-- 1. tasks.planned_at: when a ticket's scope was last set (created, restored
--    from the trash, or its name/description/acceptance criteria changed).
--    A plan approval covers tickets planned before it. Existing tickets were
--    planned when they were created.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS planned_at TIMESTAMPTZ;
UPDATE tasks SET planned_at = created_at WHERE planned_at IS NULL;
ALTER TABLE tasks ALTER COLUMN planned_at SET DEFAULT NOW();
ALTER TABLE tasks ALTER COLUMN planned_at SET NOT NULL;

-- 2. Plan approval is on by default for new projects.
ALTER TABLE projects ALTER COLUMN require_plan_approval SET DEFAULT TRUE;

-- 3. ...and turned on for every existing project. Projects that didn't
--    require approval had their plans effectively approved already (agents
--    could dispatch anything), so those plans are recorded as approved now,
--    with an audit row, and only work planned after the upgrade waits for a
--    person. Projects that already required approval keep their approval
--    state: tickets awaiting it still await it.
INSERT INTO audit_log (actor_email, org_id, project_id, action, target_type, target_id, summary, details)
SELECT 'migration 023', org_id, id, 'plan.approve', 'project', id::text,
       'plan approval turned on; the plan as it stood was recorded as approved (grandfathered by migration 023)',
       jsonb_build_object('channel', 'migration', 'grandfathered', TRUE,
                          'previous_plan_approved_at', plan_approved_at)
FROM projects
WHERE require_plan_approval = FALSE;

UPDATE projects
SET plan_approved_at = NOW(), plan_approved_by = NULL, require_plan_approval = TRUE, updated_at = NOW()
WHERE require_plan_approval = FALSE;
