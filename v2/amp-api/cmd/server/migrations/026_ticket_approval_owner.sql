-- Whom a ticket's plan approval was for (AMP #2218).
--
-- A per-ticket approval (025) belongs to the person whose ticket it was when
-- it was approved: the owner, or for an unassigned ticket the person who
-- planned it. Without recording that, a Member could claim or reassign
-- someone's ticket, approve it as their own, and hand it back approved (or
-- approve their own ticket and hand it to someone who never approved it).
-- approved_for records that person; when a Member (not a project lead or
-- above) moves the ticket to a different person, the approval is dropped.
-- NULL with approved_at set: a ticket nobody owned or planned (legacy rows).
--
-- Additive and idempotent: one new nullable column, and a backfill that only
-- fills approved rows still NULL, from the ticket's owner (or creator) now.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS approved_for INT REFERENCES users(id) ON DELETE SET NULL;

UPDATE tasks
SET approved_for = COALESCE(owner_user_id, created_by)
WHERE approved_at IS NOT NULL
  AND approved_for IS NULL
  AND COALESCE(owner_user_id, created_by) IS NOT NULL;
