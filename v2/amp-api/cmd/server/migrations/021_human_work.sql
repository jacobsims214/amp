-- Human work: a ticket is done by an agent (dispatched) or by a person (its
-- owner starts and finishes it in AMP). Human tickets sit in the dependency
-- graph like any other, so agent work that needs a person waits for them.
-- ready_at is when a ticket last became ready, so reports can tell waiting
-- to be picked up (ready → started) from working (started → done).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS performer TEXT NOT NULL DEFAULT 'agent'
    CHECK (performer IN ('agent', 'human'));
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS human_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS ready_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_tasks_human ON tasks(project_id) WHERE performer = 'human';

-- What a person reports having spent on a ticket (elapsed time includes
-- waiting; this is the work itself). Agents' runs leave it empty.
ALTER TABLE task_runs ADD COLUMN IF NOT EXISTS hours_worked NUMERIC(8, 2);
