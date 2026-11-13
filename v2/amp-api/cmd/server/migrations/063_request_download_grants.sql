-- Team requests: short-lived, single-use attachment download grants (epic 202,
-- story 549, AMP #3963; approved product contract artifact 36 v2 §1 and decision
-- D3; build slice G3 of discoveries/team-inbox-approved-build-slices.md — the
-- serving half).
--
-- 056 stores a requester's private bytes in the row. Nothing may serve them but
-- the download gate, and the contract's rule for that gate is one sentence: an
-- attachment is "served only via short-lived signed URLs, always as an
-- attachment". This migration is the row that makes a URL mean something.
--
-- WHY A ROW AT ALL, WHEN THE URL IS SIGNED. Three of the ticket's four verbs need
-- state the URL alone cannot carry:
--
--   * single-use. A signed URL that can be replayed until it expires is a shared
--     secret with a window; one that dies on first use is a download. Only a row
--     can remember the first use, and only an UPDATE ... WHERE used_at IS NULL
--     makes two concurrent downloads yield one body (see
--     repository.ConsumeRequestDownloadGrant).
--   * revocation. "Revoked grants are refused" has to survive a replica restart
--     and a clock: a deleted grant row is refused because it cannot be found, and
--     a revoked one is refused because its row says so. A signed token with a
--     short TTL could only be revoked by rotating the key under every other
--     outstanding URL.
--   * the person. A grant is bound to the user it was issued to, so possession of
--     the URL is never sufficient: the serve path re-checks who is asking (and
--     whether they may still see the request). That binding has to be stored.
--
-- WHAT THIS TABLE IS NOT:
--   * not a capability for a request, a team or a prefix. Every row names exactly
--     one attachment and one request; there is no column that could express
--     "everything", and no wildcard value. `attachment_id` is NOT NULL and > 0.
--   * not a public link. `user_id` is NOT NULL, so a row that names no person
--     cannot be stored — an anonymous URL is the thing the contract forbids.
--   * not a place a token is kept. Only `grant_hash`, the sha256 of the token, is
--     stored (the api_tokens and receipt-upload posture): a database dump holds no
--     usable URL, and the token never reaches an audit row, a log line or an event.
--   * not long-lived. `expires_at` is checked on every use, and the service mints
--     five minutes (domain.RequestDownloadGrantTTL). There is no "forever" row.
--
-- DELETION: an attachment dies with its request (056), and a grant dies with its
-- attachment — otherwise a grant row would outlive the bytes it names and a URL
-- would 404 its way through the whole TTL. `user_id` is ON DELETE SET NULL in the
-- house style, but a grant whose user is gone must never become usable by anyone
-- else: the serve path requires the *authenticated caller* to match, and a NULL
-- user matches nobody (service's check is `g.UserID != p.UserID`, so 0 never
-- matches an authenticated principal). The row simply expires unspent.
--
-- Additive and idempotent: one table, its indexes and nothing else altered.
-- Re-running this file by hand is a no-op.

CREATE TABLE IF NOT EXISTS request_download_grants (
    id            BIGSERIAL PRIMARY KEY,

    -- The token's sha256, which is how the row is found. Lower-case hex only:
    -- same posture as request_attachments.sha256 and api_tokens.token_hash.
    grant_hash    TEXT NOT NULL
        CONSTRAINT request_download_grants_hash_shape CHECK (grant_hash ~ '^[0-9a-f]{64}$')
        CONSTRAINT request_download_grants_hash_unique UNIQUE,

    -- The one file, its request and their tenant. attachment_id is NOT NULL on
    -- purpose: a grant with no attachment is a grant to nothing (or to
    -- everything), and neither may be storable. request_id/org_id/team_id are
    -- denormalised from the attachment so the serve path can refuse a URL whose
    -- numbers disagree with the row AND so a tenant's grants can be revoked in
    -- one statement without a join.
    attachment_id BIGINT NOT NULL
        CONSTRAINT request_download_grants_attachment_positive CHECK (attachment_id > 0)
        REFERENCES request_attachments(id) ON DELETE CASCADE,
    request_id    INT NOT NULL
        CONSTRAINT request_download_grants_request_positive CHECK (request_id > 0)
        REFERENCES team_requests(id) ON DELETE CASCADE,
    org_id        INT NOT NULL,
    team_id       INT NOT NULL,

    -- Who may spend it. A grant without a person is a public URL; the column is
    -- NOT NULL so that row cannot exist. ON DELETE SET NULL keeps the row (and its
    -- expiry) when the person is deleted, and service.RequireDownloadGrant matches
    -- the authenticated user id against it, so a nulled row matches nobody.
    user_id       INT REFERENCES users(id) ON DELETE SET NULL,

    -- Who minted it, when that is someone other than the recipient.
    issued_by     INT REFERENCES users(id) ON DELETE SET NULL,
    issued_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ NOT NULL,

    -- Spent. A spent grant is refused even inside its window: one URL, one body.
    used_at       TIMESTAMPTZ,

    CONSTRAINT request_download_grants_expiry_after_issue CHECK (expires_at > issued_at),
    CONSTRAINT request_download_grants_used_after_issue CHECK (used_at IS NULL OR used_at >= issued_at)
);

-- The serve path's only read: one grant by its token hash.
CREATE INDEX IF NOT EXISTS idx_request_download_grants_expiry
    ON request_download_grants (expires_at);

-- Revocation by person and by request ("the requester's access was removed",
-- "this request was closed and its files were purged") without a join.
CREATE INDEX IF NOT EXISTS idx_request_download_grants_user
    ON request_download_grants (user_id) WHERE used_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_request_download_grants_attachment
    ON request_download_grants (attachment_id);
CREATE INDEX IF NOT EXISTS idx_request_download_grants_request
    ON request_download_grants (request_id);
