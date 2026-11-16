-- Team requests: the durable record behind a team workstream inbox (epic 202,
-- story 521, AMP #3755; approved product contract = artifact 36 v2 §1, build
-- slice A1 of discoveries/team-inbox-approved-build-slices.md).
--
-- A request is not a ticket. Tickets describe work AMP schedules; a request
-- describes something a requester asked a team for, and it has a second party
-- whose acceptance is a distinct act from the team saying the outcome exists.
-- So it gets its own record rather than a tasks row with flags: no dependency
-- DAG, no dispatch, no worker runs at request level. When work is planned for
-- a request it becomes ordinary tickets, linked from request_links (a later
-- migration, slice A3).
--
-- This migration adds team_requests only; discussion/history (request_comments,
-- request_events) and links (request_links) follow in their own migrations so
-- each one is a single reviewable concern.
--
-- Identity. `id` is AMP's serial; `public_ref` is the opaque, unguessable token
-- a requester sees (contract D1). It is never the serial and never a per-team
-- sequence, and the CHECK below pins the shape so a guessable value cannot be
-- stored by mistake: the mockups' display form "REQ-8F2Q1" is a rendering, not
-- the stored token.
--
-- Tenancy. A request belongs to exactly one org and one team, and the team must
-- belong to that org. That is enforced by a composite foreign key, not by
-- convention, so no write path can create a request whose team is in another
-- tenant. The unique index on teams (id, org_id) exists only to give that
-- foreign key something to reference (id alone is already unique).
--
-- Requester. `requester_user_id` names a registered user when there is one, but
-- the requester's contact details are snapshotted into `requester_contact` at
-- submit time (contract D2) and survive the user's deletion: deleting the user
-- nulls the link (ON DELETE SET NULL) and leaves the request and its snapshot
-- intact. Nothing cascades a request away because its requester left, and the
-- contact column itself is required to be non-empty on every request.
--
-- Triage claim vs assignee. These are deliberately two fields with different
-- meanings, the same split tasks.owner_user_id has against assigned_to:
--   claim_user_id/claimed_at  "I am looking at it right now" — soft, one at a
--                             time, released freely, never auto-expires (D4).
--   owner_user_id             the person who owns the outcome — durable and
--                             transferred explicitly (a transfer clears any
--                             active claim; the service writes the event).
-- Conflating them is what made "mine" ambiguous in the human-work redesign, so
-- the database keeps them apart.
--
-- Source identity. `source_kind` says which surface submitted the request and
-- `source_external_id` is the submitting system's own immutable id (contract
-- D12: the vendor's object id, never a key or a URL). Uniqueness is per team
-- and source and applies only to non-empty ids: an API or MCP caller that sends
-- no external id must not have its requests deduplicated against every other
-- such submission, which is exactly what a plain UNIQUE would do with '' as the
-- "no id" value. A connector replaying the same external id is a no-op (the
-- github_hook_deliveries rule); a second web/MCP submission with no id is a new
-- request, and a connector must always send its id.
--
-- Lifecycle. Nine states, one fixed set enforced by CHECK (the house style —
-- tasks.merge_state, task_receipts.state), with the legal transitions owned by
-- one service function later (slice S5):
--
--   new -> triaging -> (needs_information <->) -> accepted -> in_progress
--       -> delivered -> requester_accepted
--   plus terminal rejected and duplicate; reopen is a recorded transition
--   (reopened_at/reopened_count), not a state.
--
-- The CHECKs below are backstops for the invariants a single row must satisfy
-- whatever wrote it: a rejected request says why, a delivered request has an
-- outcome brief and a delivery time, an accepted request has the requester's
-- acceptance time, and a needs_information request records since when. They do
-- not implement the transition graph (that is the service's job), and nothing
-- here infers state from linked tickets.
--
-- Care with ON DELETE SET NULL: every FK to users is SET NULL, so a CHECK must
-- still hold once that column becomes NULL. The two "someone did this" checks
-- below are therefore written as one-way implications (an actor requires its
-- timestamp; the timestamp may outlive the actor), never as biconditionals.
--
-- Timestamps that record entering a state are first-write-wins, like
-- tasks.merged_at in 038: a second write never moves them. `rev` is the
-- optimistic-concurrency token for versioned edits (a stale write is a 409, the
-- kb_document ExpectVersion pattern).
--
-- Additive and idempotent: one new table, one new index on teams, nothing
-- existing is changed.

-- The composite foreign key needs a unique key to point at. (Teams.id is
-- already unique on its own; this index exists so (id, org_id) can be a
-- foreign-key target, and it makes "this team is in this org" checkable in one
-- constraint.)
CREATE UNIQUE INDEX IF NOT EXISTS uq_teams_id_org ON teams (id, org_id);

CREATE TABLE IF NOT EXISTS team_requests (
    id                 SERIAL PRIMARY KEY,
    -- The requester-facing reference (D1): opaque, unguessable, stable. The
    -- shape CHECK is the schema's promise that a guessable sequence (or the
    -- "REQ-8F2Q1" display form) can never be stored here.
    public_ref         TEXT NOT NULL
        CONSTRAINT team_requests_public_ref_shape
        CHECK (public_ref ~ '^[A-Za-z0-9_-]{16,64}$'),

    org_id             INT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    team_id            INT NOT NULL,
    -- The team must belong to the request's org: tenant consistency is a
    -- constraint, not a convention in the write path.
    CONSTRAINT team_requests_team_in_org
        FOREIGN KEY (team_id, org_id) REFERENCES teams (id, org_id) ON DELETE CASCADE,

    -- Who asked. The snapshot is what a read shows after the user is gone.
    requester_user_id  INT REFERENCES users(id) ON DELETE SET NULL,
    requester_contact  TEXT NOT NULL DEFAULT ''
        CONSTRAINT team_requests_requester_contact
        CHECK (btrim(requester_contact) <> '')
        CONSTRAINT team_requests_requester_contact_length
        CHECK (length(requester_contact) <= 500),

    -- Which surface, and that surface's own immutable id for the object.
    source_kind        TEXT NOT NULL DEFAULT 'web'
        CONSTRAINT team_requests_source_kind
        CHECK (source_kind IN ('web', 'mcp', 'api', 'connector')),
    source_external_id TEXT NOT NULL DEFAULT ''
        CONSTRAINT team_requests_source_external_id_length
        CHECK (length(source_external_id) <= 255),
    -- A connector always carries the vendor's immutable object id; without it
    -- its replays could never be recognised (D12).
    CONSTRAINT team_requests_connector_has_external_id
        CHECK (source_kind <> 'connector' OR btrim(source_external_id) <> ''),

    -- What was asked and (later) what was delivered. Generous backstops; the
    -- product's real bounds live in the domain validation.
    subject            TEXT NOT NULL
        CONSTRAINT team_requests_subject_not_blank CHECK (btrim(subject) <> '')
        CONSTRAINT team_requests_subject_length CHECK (length(subject) <= 1000),
    input_brief        TEXT NOT NULL DEFAULT ''
        CONSTRAINT team_requests_input_brief_length CHECK (length(input_brief) <= 200000),
    outcome_brief      TEXT NOT NULL DEFAULT ''
        CONSTRAINT team_requests_outcome_brief_length CHECK (length(outcome_brief) <= 200000),

    -- The fixed lifecycle. One of the nine, nothing else.
    state              TEXT NOT NULL DEFAULT 'new'
        CONSTRAINT team_requests_state CHECK (state IN (
            'new', 'triaging', 'needs_information', 'accepted', 'in_progress',
            'delivered', 'requester_accepted', 'rejected', 'duplicate')),

    -- Triage claim: who is looking at it now (never the same thing as owner).
    claim_user_id      INT REFERENCES users(id) ON DELETE SET NULL,
    claimed_at         TIMESTAMPTZ,
    CONSTRAINT team_requests_claimed_at
        CHECK (claim_user_id IS NULL OR claimed_at IS NOT NULL),

    -- The durable assignee: the one person who owns the outcome.
    owner_user_id      INT REFERENCES users(id) ON DELETE SET NULL,

    -- Duplicate handling (D7). Set only by the duplicate transition, flattened
    -- at write time so a canonical is never itself a duplicate. The tenant
    -- foreign key that keeps a duplicate inside its own org is added below,
    -- once the (id, org_id) unique index it references exists.
    -- NO ACTION (the default) rather than SET NULL or CASCADE: a duplicate is
    -- its own request with its own requester, so deleting the canonical must
    -- neither delete it nor leave it claiming a canonical that no longer
    -- exists. The service detaches a duplicate before removing its canonical
    -- (D7's reopen rule), and a team's or org's cascade removes both in one
    -- statement, which the end-of-statement check allows.
    duplicate_of       INT,
    CONSTRAINT team_requests_duplicate_not_self
        CHECK (duplicate_of IS NULL OR duplicate_of <> id),

    -- Why a request was declined. Typed so it can be reported on.
    reject_reason      TEXT NOT NULL DEFAULT ''
        CONSTRAINT team_requests_reject_reason_length CHECK (length(reject_reason) <= 2000),

    -- State-entry timestamps, first-write-wins.
    needs_info_since   TIMESTAMPTZ,          -- D8: so clocks can exclude this time later
    planned_at         TIMESTAMPTZ,          -- first planned_as link
    delivered_at       TIMESTAMPTZ,
    accepted_at        TIMESTAMPTZ,
    accepted_by        INT REFERENCES users(id) ON DELETE SET NULL,
    closed_at          TIMESTAMPTZ,          -- first entry into a terminal state
    reopened_at        TIMESTAMPTZ,
    reopened_count     INT NOT NULL DEFAULT 0
        CONSTRAINT team_requests_reopened_count CHECK (reopened_count >= 0),

    -- Optimistic concurrency for versioned edits (stale write -> 409).
    rev                INT NOT NULL DEFAULT 1
        CONSTRAINT team_requests_rev CHECK (rev >= 1),

    -- Integration/reporting extension keys only; never an ACL or a lifecycle
    -- guarantee (domain/metadata.go limits apply on write).
    metadata           JSONB NOT NULL DEFAULT '{}'
        CONSTRAINT team_requests_metadata_object CHECK (jsonb_typeof(metadata) = 'object'),

    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Recording who accepted requires when they accepted (the timestamp, not
    -- the actor, is what the state rule reads; the actor may later be deleted).
    CONSTRAINT team_requests_acceptance_actor
        CHECK (accepted_by IS NULL OR accepted_at IS NOT NULL),
    -- Delivered is evidenced: an outcome brief and a delivery time (I2).
    CONSTRAINT team_requests_delivered_evidenced
        CHECK (state NOT IN ('delivered', 'requester_accepted')
            OR (btrim(outcome_brief) <> '' AND delivered_at IS NOT NULL)),
    -- Accepted means the requester accepted a delivery, so it was delivered.
    CONSTRAINT team_requests_accepted_requires_acceptance
        CHECK (state <> 'requester_accepted' OR accepted_at IS NOT NULL),
    -- Asking the requester something records when the wait began.
    CONSTRAINT team_requests_needs_info_since
        CHECK (state <> 'needs_information' OR needs_info_since IS NOT NULL),
    -- A declined request says why (I3).
    CONSTRAINT team_requests_rejected_reason
        CHECK (state <> 'rejected' OR btrim(reject_reason) <> ''),
    -- A duplicate names the canonical request it was folded into (I3).
    CONSTRAINT team_requests_duplicate_target
        CHECK (state <> 'duplicate' OR duplicate_of IS NOT NULL)
);

-- The opaque reference is the only thing external requesters hold; it is unique
-- across all teams so a ref alone identifies a request.
CREATE UNIQUE INDEX IF NOT EXISTS uq_team_requests_public_ref ON team_requests (public_ref);

-- The asymmetric foreign key's target (see duplicate_of above): it makes
-- "this duplicate's canonical is in the same org" a constraint.
CREATE UNIQUE INDEX IF NOT EXISTS uq_team_requests_id_org ON team_requests (id, org_id);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'team_requests_duplicate_same_tenant'
           AND conrelid = 'team_requests'::regclass
    ) THEN
        ALTER TABLE team_requests ADD CONSTRAINT team_requests_duplicate_same_tenant
            FOREIGN KEY (duplicate_of, org_id) REFERENCES team_requests (id, org_id);
    END IF;
END $$;

-- Source identity (D12 / the #3654 C3 replay rule): a connector's replay of one
-- external object collapses to one request. Empty external ids are excluded, so
-- unrelated web/MCP submissions without an id are never deduplicated against
-- each other.
CREATE UNIQUE INDEX IF NOT EXISTS uq_team_requests_source_external
    ON team_requests (team_id, source_kind, source_external_id)
    WHERE source_external_id <> '';

-- The team queue: newest first, filtered by state.
CREATE INDEX IF NOT EXISTS idx_team_requests_queue
    ON team_requests (team_id, state, id DESC);
-- A requester's own list, and their history.
CREATE INDEX IF NOT EXISTS idx_team_requests_requester
    ON team_requests (requester_user_id, created_at DESC)
    WHERE requester_user_id IS NOT NULL;
-- "Claimed by me / assigned to me" panels.
CREATE INDEX IF NOT EXISTS idx_team_requests_claim
    ON team_requests (claim_user_id) WHERE claim_user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_team_requests_owner
    ON team_requests (owner_user_id) WHERE owner_user_id IS NOT NULL;
-- Tenant-level reporting across a team's requests.
CREATE INDEX IF NOT EXISTS idx_team_requests_org
    ON team_requests (org_id, created_at DESC);
