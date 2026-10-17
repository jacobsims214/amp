-- Plan approval: a person approves a project's plan before agents start it.
-- Tickets created after plan_approved_at are "new since the last approval".
-- With require_plan_approval on, those can't be dispatched until someone
-- approves again. Off by default: existing projects behave as before.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS plan_approved_at TIMESTAMPTZ;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS plan_approved_by INT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS require_plan_approval BOOLEAN NOT NULL DEFAULT FALSE;
