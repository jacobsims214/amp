-- One step per merge (AMP #3009).
--
-- A human merge step names the ticket whose PR merge it tracks in its own
-- metadata (merge_for), so the plan's step and My work's Merge card for that
-- PR are one action instead of two. "One step per merge" is a rule the
-- service checks before writing (Service.checkMergeFor), but that check is
-- check-then-write: two people linking two steps to the same ticket at the
-- same moment both read "nothing tracks it yet" and both store a link,
-- leaving the duplicate the key exists to remove — and the plan warning that
-- reports it is a warning, not a repair.
--
-- This index makes the database the arbiter, so concurrent linkers converge:
-- the second write fails on the constraint and is reported as the duplicate
-- it is. It covers OPEN steps only: a completed step isn't tracking anything
-- any more (its merge is recorded), so a later step may link to the same
-- ticket without colliding with history — which is also what lets a wrong
-- link be cleared and made again.
--
-- The expression mirrors repository.mergeForSQLOf exactly: a JSON number or
-- string, with an optional "#" and surrounding spaces, bounded to a positive
-- int4 so a junk value reads as NULL (no link) instead of breaking inserts.
-- Additive and idempotent: no column, no backfill, and every ticket without
-- merge_for contributes a NULL key, which unique indexes don't compare.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_merge_for_open ON tasks
    ((CASE WHEN btrim(COALESCE(metadata ->> 'merge_for', ''), '# ') ~ '^[0-9]{1,10}$'
             AND btrim(COALESCE(metadata ->> 'merge_for', ''), '# ')::numeric <= 2147483647
           THEN btrim(COALESCE(metadata ->> 'merge_for', ''), '# ')::int END))
    WHERE performer = 'human' AND state <> 'completed';
