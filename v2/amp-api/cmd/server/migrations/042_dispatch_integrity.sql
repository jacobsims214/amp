-- Dispatch integrity (epic 180, story 444, AMP #2801): a dispatched ticket
-- isn't started until its worker claims it.
--
-- Everything here is additive. The four task states are unchanged, so old
-- plugins, filters, reports and REST clients keep seeing in_progress as
-- before: "dispatched, not started" is derived from started_at being NULL,
-- never stored as a state.
--
-- tasks.dispatch_id   the token amp_dispatch_task hands out; the worker
--                     presents it with amp_claim_task, and a completion
--                     with it is the worker's own (without it, a closeout).
-- tasks.started_at    when the ticket was started (a claim, or the first
--                     activity of a plugin that doesn't claim). NULL on a
--                     dispatched ticket means no worker has started it.
-- tasks.started_by    the agent that started it, "inferred" for the first-
--                     activity fallback, "backfill" for rows that predate it.
-- tasks.returned_at   when the watchdog (or amp_return_task) sent a
--                     dispatched ticket back to ready; cleared on the next
--                     dispatch. returned_reason says why.
--
-- The watchdog's two timeouts live on the project: flag a not-started
-- ticket after not_started_flag_minutes (10), return it to ready after
-- not_started_return_minutes (30). 0 turns each off.
--
-- Backfill: every in_progress ticket is treated as started (by its
-- dispatched_at, else updated_at), so nothing in flight when this lands is
-- flagged or returned. Additive only; idempotent.

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS dispatch_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS started_at      TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS started_by      TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS returned_at     TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS returned_reason TEXT NOT NULL DEFAULT '';

ALTER TABLE projects ADD COLUMN IF NOT EXISTS not_started_flag_minutes   INT NOT NULL DEFAULT 10;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS not_started_return_minutes INT NOT NULL DEFAULT 30;

UPDATE tasks
   SET started_at = COALESCE(dispatched_at, updated_at),
       started_by = 'backfill'
 WHERE state = 'in_progress'
   AND started_at IS NULL;

-- The watchdog and amp_list_tasks' not_started list both scan dispatched,
-- unstarted tickets in a project; keep that cheap.
CREATE INDEX IF NOT EXISTS idx_tasks_not_started ON tasks(project_id, dispatched_at)
 WHERE state = 'in_progress' AND started_at IS NULL;

-- amp_return_task / the watchdog find returned-but-not-re-dispatched
-- tickets the same way.
CREATE INDEX IF NOT EXISTS idx_tasks_returned ON tasks(project_id, returned_at)
 WHERE returned_at IS NOT NULL;
