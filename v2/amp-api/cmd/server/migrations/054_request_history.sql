-- Team requests: discussion and history (epic 202, story 521, AMP #3757;
-- approved product contract = artifact 36 v2 §1, invariants I5 and I6; build
-- slice A2 of discoveries/team-inbox-approved-build-slices.md).
--
-- 053 added team_requests, the request record itself. This migration adds the
-- two tables that hang off it, in the shape the rest of AMP already uses for
-- the same job: `request_comments` for the thread (the `comments` shape) and
-- `request_events` for the history (the `activity_log` shape).
--
-- Why two tables and not one. A comment is something a person or a service
-- said and the requester can read; an event is what happened, for the team's
-- history rail and the audit of the lifecycle. They have different readers
-- (requester thread versus team history), different vocabularies (comment
-- kinds versus acting verbs) and different row rules, so they are not one
-- table with a discriminator.
--
-- APPEND-ONLY IS AN APPLICATION CONTRACT, NOT A SQL GUARANTEE. Rows are only
-- ever inserted: no service or repository path updates a comment's body, an
-- event's action or either row's attribution, and an edit or a retraction is a
-- NEW row. This migration deliberately ships no trigger refusing UPDATE or
-- DELETE, so the absence of one must not be read as immutability. Two writes
-- are legitimate and leave such a row changed:
--   * the database itself nulls `author_user_id`/`actor_user_id` when that
--     user is deleted (ON DELETE SET NULL, the house rule for user
--     references), and the `author`/`actor` text snapshot keeps the row
--     attributable after the fact;
--   * the parent request lifecycle deletes the rows with the request: history
--     is part of the record, so retention, close-out and the trash follow
--     team_requests (ON DELETE CASCADE below) instead of living apart from it.
-- #3757's brief says exactly this: follow the approved parent lifecycle, and
-- do not promise immutability the schema does not enforce. (`audit_log` can
-- afford a hard UPDATE/DELETE guard — migration 033 — because nothing cascades
-- into it; request history is customer-visible content of a record that is
-- deleted with its team, so the same guard would only break retention.)
--
-- Attribution. `author`/`actor` is the display name captured at write time (the
-- `comments.author` / `activity_log.actor` precedent) and the `*_user_id`
-- column is the AMP user behind it when there is one. A connector or MCP
-- service client posts as its own integration identity, with no AMP user: the
-- text column is what makes that row readable, and it survives the user's
-- deletion. Both are required and non-blank, because an unattributed row in a
-- requester-visible thread is a bug, not a state.
--
-- I5 (asking obliges an answer). `kind = 'question'` is the team asking the
-- requester something, and entering `needs_information` requires a preceding
-- one; `kind = 'answer'` is what returns the request to the state it left. The
-- schema cannot see the request row from here (that is the service transition
-- function, slice S5), but it fixes the vocabulary and gives the lookup a
-- partial index so "the latest question for this request" is one cheap query.
--
-- I6 (every act is one row). `request_events.action` is the closed verb set the
-- service writes: submit, transition, claim, release, assign, link, unlink. A
-- transition names the state it left and the state it entered (from_state /
-- to_state); every other verb names neither, so it explains itself in `detail`
-- instead. The CHECK below is the same rule domain.RequestEvent.Validate
-- enforces, so the database and the domain cannot disagree about the shape of
-- a history row. The legal transition GRAPH is not here: a CHECK cannot see the
-- previous row, and encoding it in a trigger would duplicate the one service
-- function that owns it (I7).
--
-- Additive and idempotent: two new tables and their indexes, nothing existing
-- is altered or re-indexed. Re-running this file by hand is a no-op.

-- The discussion thread. One row per thing said; the requester reads their own
-- thread and the team reads the same one.
CREATE TABLE IF NOT EXISTS request_comments (
    id             BIGSERIAL PRIMARY KEY,
    -- History belongs to the request: it dies with it (see the header).
    request_id     INT NOT NULL REFERENCES team_requests(id) ON DELETE CASCADE,
    -- What kind of remark this is. I5 turns on question/answer; 'system' is
    -- what a transition or a connector writes on the request's behalf.
    kind           TEXT NOT NULL DEFAULT 'comment'
        CONSTRAINT request_comments_kind
        CHECK (kind IN ('comment', 'question', 'answer', 'system')),
    body           TEXT NOT NULL
        CONSTRAINT request_comments_body_not_blank CHECK (btrim(body) <> '')
        -- Generous backstop; the product's bound is domain.MaxRequestBriefLen.
        CONSTRAINT request_comments_body_length CHECK (length(body) <= 200000),
    -- Attribution: the name as written, and the AMP user behind it if any.
    author         TEXT NOT NULL DEFAULT 'system'
        CONSTRAINT request_comments_author_not_blank CHECK (btrim(author) <> ''),
    author_user_id INT REFERENCES users(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The thread read: one request's comments in append order. Order by id, not
-- created_at, so two rows written in the same transaction still have a stable
-- order (and the read is keyset-friendly, the ListAudit shape).
CREATE INDEX IF NOT EXISTS idx_request_comments_request
    ON request_comments (request_id, id);

-- I5's query — "the latest question asked about this request" — without
-- scanning the thread. Partial, because questions are a small minority of rows.
CREATE INDEX IF NOT EXISTS idx_request_comments_question
    ON request_comments (request_id, id DESC) WHERE kind = 'question';

-- The history rail: what happened to a request, in order.
CREATE TABLE IF NOT EXISTS request_events (
    id          BIGSERIAL PRIMARY KEY,
    request_id  INT NOT NULL,
    -- Tenant scope, denormalized so org- and team-wide history reads need no
    -- join. Both halves are constraints, not conventions: (request_id, org_id)
    -- pins the request and its org together (053's uq_team_requests_id_org),
    -- and (team_id, org_id) pins the team into that same org (053's
    -- uq_teams_id_org). A history row can therefore never cross a tenant.
    --
    -- The two columns stay in step with the request because the write path
    -- copies them from it in the same statement; team_id is reporting scope
    -- (the history rail reads by request_id), and an exact same-team guarantee
    -- would need a unique key on team_requests (id, team_id), which this
    -- migration deliberately does not add: #3755's proof asserts the table's
    -- index set, and a later ticket adding an index there must know it does.
    org_id      INT NOT NULL,
    team_id     INT NOT NULL,
    -- The act. Closed set: the service writes one of these per act (I6).
    action      TEXT NOT NULL
        CONSTRAINT request_events_action
        CHECK (action IN ('submit', 'transition', 'claim', 'release', 'assign', 'link', 'unlink')),
    -- Set for transitions only; '' everywhere else. The same nine states as
    -- team_requests.state, because history must not invent a state.
    from_state  TEXT NOT NULL DEFAULT ''
        CONSTRAINT request_events_from_state
        CHECK (from_state = '' OR from_state IN (
            'new', 'triaging', 'needs_information', 'accepted', 'in_progress',
            'delivered', 'requester_accepted', 'rejected', 'duplicate')),
    to_state    TEXT NOT NULL DEFAULT ''
        CONSTRAINT request_events_to_state
        CHECK (to_state = '' OR to_state IN (
            'new', 'triaging', 'needs_information', 'accepted', 'in_progress',
            'delivered', 'requester_accepted', 'rejected', 'duplicate')),
    -- Who did it, and the AMP user behind the name (ON DELETE SET NULL, with
    -- the name kept — see the attribution note in the header).
    actor       TEXT NOT NULL DEFAULT 'system'
        CONSTRAINT request_events_actor_not_blank CHECK (btrim(actor) <> ''),
    actor_user_id INT REFERENCES users(id) ON DELETE SET NULL,
    -- Prose or JSON text explaining the act (a transfer's previous assignee, a
    -- link's target, a handoff's ref->id ledger). Generous backstop only.
    detail      TEXT NOT NULL DEFAULT ''
        CONSTRAINT request_events_detail_length CHECK (length(detail) <= 200000),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- A transition names both ends; every other act names neither. This is the
    -- same rule as domain.RequestEvent.Validate, so no row can claim half a
    -- transition, and no non-transition verb can smuggle in states.
    CONSTRAINT request_events_transition_states
        CHECK ((action = 'transition' AND from_state <> '' AND to_state <> '')
            OR (action <> 'transition' AND from_state = '' AND to_state = '')),

    -- The request this row is history of, in its own tenant. The cascade is
    -- the retention rule in the header: history is deleted with what it
    -- describes, never left orphaned.
    CONSTRAINT request_events_request_in_org
        FOREIGN KEY (request_id, org_id) REFERENCES team_requests (id, org_id)
        ON DELETE CASCADE,
    CONSTRAINT request_events_team_in_org
        FOREIGN KEY (team_id, org_id) REFERENCES teams (id, org_id)
        ON DELETE CASCADE
);

-- The history read for one request, in append order.
CREATE INDEX IF NOT EXISTS idx_request_events_request
    ON request_events (request_id, id);
-- "What was the last transition / claim?" and per-verb history slices.
CREATE INDEX IF NOT EXISTS idx_request_events_action
    ON request_events (request_id, action, id DESC);
-- Team-scoped history, and the index a team's deletion cascades through.
CREATE INDEX IF NOT EXISTS idx_request_events_team
    ON request_events (team_id, created_at DESC);
-- Tenant-level history and activity reporting.
CREATE INDEX IF NOT EXISTS idx_request_events_org
    ON request_events (org_id, created_at DESC);
