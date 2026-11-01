-- Remote sessions have no time limit by default (AMP #2424, user decision
-- 2026-10-26: "keep itself alive until a user disconnects it").
--
-- Before this, every session ended after 30 minutes without remote activity
-- (orgs.remote_idle_timeout_min, default 30) and 12 hours after it started
-- (remote_sessions.expires_at, always started_at + 12 h). Now both are off
-- unless an org admin opts in:
--
-- * orgs.remote_idle_timeout_min: 0 means off (the new default); 5..240 is an
--   admin's limit.
-- * orgs.remote_max_lifetime_hours (new): 0 means off (the default); 1..720
--   is an admin's limit, applied to sessions registered after it is set.
-- * remote_sessions.expires_at: NULL when the session has no lifetime.
--
-- What happens to existing values. The web UI never had an idle-timeout
-- control (its PUT sent only "enabled"), so a stored 30 is the column default
-- and not anyone's choice: it becomes 0 (off). Any other value could only have
-- been set on purpose through the API, so it is kept. An admin who wants a
-- 30 minute limit sets it again in the org settings. Live sessions lose
-- the 12 h lifetime they were given at registration (no org has a lifetime
-- after this migration).
--
-- Additive for readers that know the new meaning; runs once (versioned).
-- Rollback: an older amp-api/amp-relay can't read a NULL expires_at, so
-- roll back by setting expires_at = started_at + INTERVAL '12 hours' where it
-- is NULL and remote_idle_timeout_min = 30 where it is 0, before downgrading.

ALTER TABLE orgs ADD COLUMN IF NOT EXISTS remote_max_lifetime_hours INT NOT NULL DEFAULT 0;
ALTER TABLE orgs ALTER COLUMN remote_idle_timeout_min SET DEFAULT 0;

ALTER TABLE orgs DROP CONSTRAINT IF EXISTS orgs_remote_idle_timeout_min_range;
UPDATE orgs SET remote_idle_timeout_min = 0 WHERE remote_idle_timeout_min = 30;
ALTER TABLE orgs ADD CONSTRAINT orgs_remote_idle_timeout_min_range
    CHECK (remote_idle_timeout_min = 0 OR remote_idle_timeout_min BETWEEN 5 AND 240);

ALTER TABLE orgs DROP CONSTRAINT IF EXISTS orgs_remote_max_lifetime_hours_range;
ALTER TABLE orgs ADD CONSTRAINT orgs_remote_max_lifetime_hours_range
    CHECK (remote_max_lifetime_hours BETWEEN 0 AND 720);

ALTER TABLE remote_sessions ALTER COLUMN expires_at DROP NOT NULL;
UPDATE remote_sessions SET expires_at = NULL WHERE state = 'active';
