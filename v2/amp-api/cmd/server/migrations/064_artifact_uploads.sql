-- Artifact publishes by upload URL (AMP #4937, story 624).
--
-- amp_publish_artifact with bytes+sha256 instead of content checks the publish
-- as the MCP caller and mints a one-time PUT URL; the version is created when
-- the file lands there. Only the token's hash is stored. `caller` is the
-- minting principal (no request metadata) so the upload, which carries no
-- session, publishes as that caller and is re-authorized at upload time.
--
-- A finished row keeps artifact_id/version so a retried PUT after a lost
-- response answers with the same artifact instead of a second version.
CREATE TABLE IF NOT EXISTS artifact_uploads (
    id          BIGSERIAL PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    user_id     INTEGER REFERENCES users(id) ON DELETE CASCADE,
    caller      JSONB NOT NULL,
    request     JSONB NOT NULL,
    bytes       BIGINT NOT NULL CHECK (bytes > 0),
    sha256      TEXT NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    claimed_at  TIMESTAMPTZ,
    artifact_id INTEGER,
    version     INTEGER,
    done_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_artifact_uploads_expires ON artifact_uploads (expires_at);
