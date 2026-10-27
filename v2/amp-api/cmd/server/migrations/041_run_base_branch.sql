-- The branch a run's work went into (AMP #2596): a PR's target, or the
-- branch the work was merged or pushed into. With a ticket's
-- integration_branch metadata it decides whether the run holds the
-- ticket's dependents: work that landed on the integration branch doesn't
-- (the integration branch's own PR to main still does).
--
-- Additive only, NOT NULL with an empty default, no backfill: runs recorded
-- before it read as "not said", and old replicas never write it. Idempotent.
ALTER TABLE task_runs ADD COLUMN IF NOT EXISTS base_branch TEXT NOT NULL DEFAULT '';
