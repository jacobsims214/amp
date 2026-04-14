-- Add optional start_at to tasks for scheduled/deferred execution.
-- NULL means no schedule constraint (existing behavior unchanged).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS start_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_tasks_start_at ON tasks(start_at) WHERE start_at IS NOT NULL;
