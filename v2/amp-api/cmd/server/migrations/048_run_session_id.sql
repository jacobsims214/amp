-- The remote-control session a run happened in (#131), so a ticket's run
-- links to /sessions/<id>, the session that produced it. Recorded by
-- harnesses that know it (the relay harness); empty for every other run.
--
-- Additive only, NOT NULL with an empty default, no backfill: runs recorded
-- before it read as "no session", and old replicas never write it.
-- Idempotent. Numbered 048 to stay clear of 046/047 on the decision-docs
-- and verification-receipts branches.
ALTER TABLE task_runs ADD COLUMN IF NOT EXISTS session_id TEXT NOT NULL DEFAULT '';
