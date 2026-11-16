-- Team requests: the submit idempotency key (AMP #3871, epic 202, story 535;
-- approved product contract artifact 36 v2 §5 — "Idempotency key on submit
-- (replay returns the same request)" — and decision D12 for connector external
-- ids; build slice C1 of discoveries/team-inbox-approved-build-slices.md).
--
-- The problem. A submit is an HTTP/MCP call that can lose its response: the
-- client retries and, without a key, the requester's second delivery is a second
-- request in the team's queue — the duplicate a request is supposed to make
-- impossible. 053 already gives *connector* intake a natural key (the vendor's
-- immutable object id, `uq_team_requests_source_external`); a web form, an MCP
-- tool call or an API client has no such id, so the client supplies one and this
-- table remembers what it produced.
--
-- The scope is the whole identity of a submission, not just the token:
-- (org, team, source_kind, submitter, key). Three consequences, all deliberate:
--
--   * Two people cannot collide. Idempotency keys are client-chosen and often
--     derived (`<form-id>-<draft-id>`), so a key that is unique per person in one
--     browser is not unique across a team's requesters. Scoping by submitter
--     means one person's key can never address, replay or reveal another
--     person's request — the refusal that matters most here.
--   * One team's keys are another team's private. A key replayed against a
--     different team (or org) creates that team's own request instead of
--     answering with one whose content the caller may not read.
--   * The source is part of it, so a web key and an MCP key are separate
--     namespaces and a connector's key cannot shadow a form's.
--
-- The row is written in the SAME TRANSACTION as the request it answers for, and
-- `request_id` is NOT NULL. That is the whole design: there is no "pending" /
-- "claimed" state, so there is nothing for a failed attempt to leave behind. An
-- attempt that dies between the request insert and the commit rolls both rows
-- back, and the client's retry simply creates the request. The
-- `ClaimGitHubDelivery` shape (insert with status 0, update it afterwards) is
-- deliberately NOT reused for request creation: it exists there because a
-- delivery must record an attempt that produced nothing, and the stuck-claim
-- state it tolerates is exactly what a submit retry must not inherit.
--
-- Duplicate-key semantics live in the write path, not here: the unique index is
-- the arbiter (two concurrent submissions with one key: exactly one inserts, the
-- other's transaction fails on this index and is resolved into a replay of the
-- committed row), and a replay with a *different* normalized payload is a
-- conflict rather than a silent reuse of the earlier request. `payload_hash` is
-- what makes that comparison possible without re-deriving the request's content:
-- it is a sha256 over the normalized submission (domain request validation's
-- output), so " Hello " and "Hello" are the same payload and an edited brief is
-- not.
--
-- Tenancy is a constraint, not a convention. `(request_id, org_id)` points at
-- 053's `uq_team_requests_id_org`, so a key can only ever name a request in its
-- own org, and `(team_id, org_id)` points at `uq_teams_id_org`, so the team must
-- be in the same org. A write path — or a hand fix — that mixes tenants is
-- refused by the database rather than stored.
--
-- Retention follows the request: a deleted request takes its key rows with it
-- (ON DELETE CASCADE), because a key whose request is gone must not answer a
-- retry with an id that no longer exists; the retry then creates a fresh
-- request, which is the honest outcome. Deleting the submitter does the same
-- (ON DELETE CASCADE): a key belongs to one person, and a person who no longer
-- exists has no retries to serve. `created_at` is the attempt's time, for
-- support and for a future prune job; nothing reads it to decide a replay.
--
-- Additive and idempotent: one new table, its unique index and one index for the
-- cascade. Nothing existing is altered, so 053's pinned `team_requests` index
-- set is untouched and re-running this file by hand changes nothing.

CREATE TABLE IF NOT EXISTS request_idempotency_keys (
    id                BIGSERIAL PRIMARY KEY,

    -- The submission's tenant, in both halves. The two foreign keys at the
    -- bottom make each half a constraint against the right table.
    org_id            INT NOT NULL,
    team_id           INT NOT NULL,

    -- Which surface the key belongs to, and the person behind it. The submitter
    -- is the request's requester (the authenticated principal), never a name or
    -- an address from the payload: the point of scoping by it is that no
    -- client-supplied value can change whose namespace a key lands in.
    source_kind       TEXT NOT NULL
        CONSTRAINT request_idempotency_keys_source_kind
        CHECK (source_kind IN ('web', 'mcp', 'api', 'connector')),
    submitter_user_id INT NOT NULL
        CONSTRAINT request_idempotency_keys_submitter
        REFERENCES users(id) ON DELETE CASCADE,

    -- The client's token. 255 is the conventional ceiling for an
    -- Idempotency-Key header; it is stored as sent (bounded and non-blank),
    -- because it is the client's own opaque value — not a credential, and the
    -- unique index below is what it means.
    idempotency_key   TEXT NOT NULL
        CONSTRAINT request_idempotency_keys_key_not_blank CHECK (btrim(idempotency_key) <> '')
        CONSTRAINT request_idempotency_keys_key_length CHECK (length(idempotency_key) <= 255),

    -- sha256 hex of the normalized submission (the write path's fingerprint).
    -- Compared on replay: the same key with a different payload is a conflict.
    payload_hash      TEXT NOT NULL
        CONSTRAINT request_idempotency_keys_payload_hash
        CHECK (payload_hash ~ '^[0-9a-f]{64}$'),

    -- The request this key produced. Never NULL, never pending (see the header).
    request_id        INT NOT NULL,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- The scope, and the arbiter of a same-key race: exactly one submission per
    -- (tenant, source, submitter, key) can be stored, so a concurrent replay is a
    -- unique violation the write path resolves into a replay.
    CONSTRAINT request_idempotency_keys_scope
        UNIQUE (org_id, team_id, source_kind, submitter_user_id, idempotency_key),

    -- The key names a request in ITS org (053's uq_team_requests_id_org), and it
    -- dies with that request: a retry must never be answered with a deleted id.
    CONSTRAINT request_idempotency_keys_request_in_org
        FOREIGN KEY (request_id, org_id) REFERENCES team_requests (id, org_id)
        ON DELETE CASCADE,

    -- The team must be the tenant's own (053's uq_teams_id_org).
    CONSTRAINT request_idempotency_keys_team_in_org
        FOREIGN KEY (team_id, org_id) REFERENCES teams (id, org_id)
        ON DELETE CASCADE
);

-- The foreign key's referencing side: deleting a request cascades through this
-- index instead of scanning the table.
CREATE INDEX IF NOT EXISTS idx_request_idempotency_keys_request
    ON request_idempotency_keys (request_id);

-- The one question a support or prune job asks that the scope index does not
-- serve: "which keys belong to this team" (the scope index leads with org, which
-- is the same set for a team but ordered differently; this one is cheap and
-- explicit).
CREATE INDEX IF NOT EXISTS idx_request_idempotency_keys_team
    ON request_idempotency_keys (team_id, created_at DESC);
