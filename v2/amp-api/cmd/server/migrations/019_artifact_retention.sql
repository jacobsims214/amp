-- Artifacts expire: unless a person keeps one, it's deleted a while after its
-- last version (ARTIFACT_RETENTION_DAYS, default 45). Its creator and the
-- ticket's owner are warned on their home page first, and can keep it or let
-- it go (dismissing the warning).
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS keep BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS kept_by INT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS kept_at TIMESTAMPTZ;
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS expiry_dismissed_at TIMESTAMPTZ;
UPDATE artifacts SET expires_at = updated_at + INTERVAL '45 days' WHERE expires_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_artifacts_expiry ON artifacts(expires_at) WHERE NOT keep;
