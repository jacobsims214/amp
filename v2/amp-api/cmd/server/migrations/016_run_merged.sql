-- Whether a run's work is merged into the main line. Completed tickets whose
-- latest pushed work isn't merged are the "waiting to merge" queue: done by
-- the agent, waiting on a person.
ALTER TABLE task_runs ADD COLUMN IF NOT EXISTS merged BOOLEAN;
