-- Sealed project GitHub credentials (epic 183, story 453, AMP #3154).
--
-- Migration 044 created project_github_credentials as reference metadata
-- only: host, label, who attached it and when, and nothing to read back. It
-- deliberately had no secret-bearing column, because the token itself is
-- this migration's subject. This adds the one place a project's GitHub token
-- lives, and the one rule that keeps it from ever being stored in the clear.
--
-- WHY A COLUMN HERE AND NOT A SECOND TABLE. The credential is one row per
-- project and host (044's UNIQUE (project_id, host)), and the facts cache
-- references it through the composite foreign key
-- (credential_id, project_id, host). A second table would mean a second row
-- to keep in step with the first, and a moment where a credential exists in
-- one and not the other — a facts row could then reference a credential that
-- has no secret, or a secret could outlive the credential the facts point
-- at. One row keeps "the credential is attached" and "there is a token for
-- it" the same fact, and deleting the row (044: revoked credentials are
-- deleted, not flagged) still cascades to the facts it produced.
--
-- SEALED ONLY, ENFORCED HERE. The value is sealed by internal/secretbox
-- (AES-256-GCM, purpose "project_github_credentials.sealed_secret"), the
-- same pattern as project_github_hooks.secret (039) and webhook auth
-- headers (#2441). The CHECK below is the part that matters: a value that
-- is not a sealed value is refused by the database, so a code path that
-- forgets to seal — or a hand-written INSERT in a hurry — cannot put a
-- plaintext token in a row. That is also why this column is TEXT and not
-- BYTEA or JSONB: what is stored is the same "enc:v1:<key id>:<base64url>"
-- text every other sealed value in AMP is, so key rotation and the startup
-- reseal work on it unchanged.
--
-- Changing the sealed prefix (secretbox.Prefix) needs this migration in the
-- same change, on purpose: the database would otherwise refuse every write.
--
-- '' IS ALLOWED. A credential row with no sealed secret is the pre-#3154
-- state (044's reference metadata, and the rows 044's own tests insert): it
-- records that a credential context exists without a token for it. The
-- storage layer never writes '' — a token is required to create or rotate
-- one — and a facts row read with such a credential is treated by the
-- classifier as a weaker claim, exactly as credential_id NULL is.
--
-- Additive (one column, no backfill: existing rows get '', which is already
-- their meaning) and idempotent, matching the project's no-down-migration
-- rule: an older replica that doesn't know this column keeps working during
-- a rollout because the default supplies it.
ALTER TABLE project_github_credentials
    ADD COLUMN IF NOT EXISTS sealed_secret TEXT NOT NULL DEFAULT '';

ALTER TABLE project_github_credentials
    DROP CONSTRAINT IF EXISTS project_github_credentials_sealed_check;

ALTER TABLE project_github_credentials
    ADD CONSTRAINT project_github_credentials_sealed_check
    CHECK (sealed_secret = '' OR sealed_secret LIKE 'enc:v1:%');
