-- Per-ticket plan approval (AMP #2198, option D of #2197).
--
-- Approval was one watermark per project (projects.plan_approved_at): one
-- person's click approved everyone's tickets planned before it. Now each
-- ticket records its own approval: approved_at (when a person approved it)
-- and approved_by (who). A ticket is approved iff approved_at is set and
-- approved_at >= planned_at, so re-planning it (a material edit, a restore
-- from the trash) makes it wait again. projects.plan_approved_at/by stay, as
-- the time and person of the project's last approval, but no longer gate.
--
-- Additive and idempotent: new nullable columns, and a backfill that only
-- fills rows still NULL.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS approved_by INT REFERENCES users(id) ON DELETE SET NULL;

-- Backfill: every ticket the project-wide approval covered (planned at or
-- before plan_approved_at) keeps being approved, as of that approval and by
-- that person. approved_by stays NULL where the project's was NULL (the
-- plans grandfathered by migration 023): NULL with approved_at set means
-- "approved by a migration".
UPDATE tasks t
SET approved_at = p.plan_approved_at, approved_by = p.plan_approved_by
FROM projects p
WHERE t.project_id = p.id
  AND t.approved_at IS NULL
  AND p.plan_approved_at IS NOT NULL
  AND t.planned_at <= p.plan_approved_at;
