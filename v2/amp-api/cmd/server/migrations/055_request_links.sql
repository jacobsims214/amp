-- Team requests: the durable request-to-work lineage (epic 202, story 521,
-- AMP #3758; approved product contract = artifact 36 v2 §3, build slice A3 of
-- discoveries/team-inbox-approved-build-slices.md).
--
-- 053 added the request record and 054 added its discussion and history. This
-- migration adds the relation between a request and the work that serves it:
-- the project it is routed to, the epics/stories/tasks its plan is made of, the
-- tasks/artifacts/receipts that delivered it, and the links between requests.
--
-- WHY A TABLE AND NOT METADATA. Contract §3 says planned_as links go "through a
-- durable link table, never metadata", and that matters beyond tidiness.
-- Effective metadata is inherited project -> epic -> story -> task, so a
-- `request_ref` key set on a story would claim every ticket beneath it as
-- belonging to that request (amp_find_by_metadata reads effective metadata and
-- cannot tell inherited from own — the same trap `merge_for` documents). It is
-- also a JSONB value an ordinary metadata patch can overwrite, so the lineage
-- would be mutable without any row of the relation changing. A typed relation
-- row cannot be inherited, cannot be clobbered by a metadata patch, and cannot
-- be read as belonging to the wrong request. Metadata keeps its real job
-- (`request_ref` as a search label); the relation lives here.
--
-- WHY NO FOREIGN KEY ON target_id. A link outlives the thing it names. If the
-- target were an FK, deleting a ticket would either erase the row (CASCADE) or
-- refuse the delete (NO ACTION), and both are wrong for lineage: "we delivered
-- this request with ticket #3218, then the plan changed and the ticket was
-- deleted" has to stay readable. So target_id is a plain INT and a row pointing
-- at a deleted target is a TOMBSTONE — the contract's deliberate choice, and
-- the same reason task_runs and task_receipts carry no FK to tasks. What the
-- row loses when its target goes is the ability to join; what it keeps is the
-- record that the relation existed.
--
-- WHAT THE ABSENT FK COSTS. Tenant safety cannot be a foreign key on the
-- target, so it must be enforceable by the authorized link service that later
-- writes these rows: the service resolves the target, checks its org against
-- `org_id`, and only then inserts. That is why the request's tenant is a real
-- column with a real composite FK rather than a metadata key — the row carries
-- the scope the service has to check against, so "same tenant" is a comparison
-- of two typed columns and never a JSONB lookup. (A CHECK cannot reach another
-- row, so the target's tenant is not enforceable here; the column set makes it
-- enforceable there.)
--
-- WHAT IS ENFORCED HERE. Everything a single row can prove:
--   * the five link kinds and seven target kinds are closed sets, matching
--     domain.RequestLinkKinds / domain.RequestTargetKinds (slice A3's enums);
--   * the kind and the target kind agree — routes_to only a project,
--     planned_as only epics/stories/tasks, delivered_by only tasks/artifacts/
--     receipts, duplicate_of and relates_to only another request — the same
--     map as domain.requestLinkTargets, so a planned_as link to an artifact or
--     a routes_to link to a ticket cannot be stored by any write path;
--   * a request does not link to itself (target_kind='request' implies a
--     different target_id), the domain's rule and a one-row invariant;
--   * target_id is positive: a link with no target names nothing;
--   * the same relation attached twice is one row — UNIQUE (request_id,
--     target_kind, target_id, link_kind). Re-attaching is an
--     `ON CONFLICT DO NOTHING` no-op (the task_dependencies pattern), so a
--     retried attach converges instead of duplicating lineage;
--   * the request and its tenant are pinned together by a composite FK to 053's
--     uq_team_requests_id_org, so a link cannot claim an org its request is not
--     in.
--
-- WHAT IS NOT ENFORCED HERE, BY DESIGN. "routes_to is one project per request"
-- (D9) and "a duplicate_of target is in the same tenant" (D7) are rules about
-- more than one row or about a row that has no FK, so they belong to the link
-- service (slice C4/E1), not to a trigger or an index here. A unique index over
-- routes_to would also make re-routing a request destroy the previous routing
-- row, and a re-route is lineage the team should be able to read back. Likewise
-- "a canonical is never a duplicate" is flattened at write time by the service.
--
-- DELETION. Unlike request_comments/request_events (054), a link IS removable:
-- the contract's `unlink` event exists precisely because a team may detach a
-- ticket from a request without deleting either. What does cascade is the
-- request's own lifecycle — the link is part of the request's lineage record, so
-- deleting the request (or its team, or its org) takes its links with it, the
-- same retention rule 054's history follows. The two are not in conflict: the
-- row dies with the REQUEST, never with the target.
--
-- Additive and idempotent: one new table and its indexes, nothing existing is
-- altered or re-indexed. Re-running this file by hand is a no-op.

CREATE TABLE IF NOT EXISTS request_links (
    id          BIGSERIAL PRIMARY KEY,
    -- The request this relation belongs to. It dies with the request: lineage is
    -- part of the request's record, so retention follows team_requests.
    request_id  INT NOT NULL,
    -- Tenant scope, denormalised so the authorized link service can check "the
    -- target is in the request's tenant" without a join, and so org-wide
    -- reporting needs none. The composite FK below pins it to the request's own
    -- org, so the column cannot drift from the row it describes.
    org_id      INT NOT NULL,
    -- What kind of relation this is: the five the contract defines (§3, D9, D10).
    -- No default: an unlabelled relation row would be unreadable.
    link_kind   TEXT NOT NULL
        CONSTRAINT request_links_link_kind CHECK (link_kind IN (
            'routes_to', 'planned_as', 'delivered_by', 'duplicate_of', 'relates_to')),
    -- What it points at. NO FOREIGN KEY here, deliberately — see the header. A
    -- link to a deleted target stays as a tombstone.
    target_kind TEXT NOT NULL
        CONSTRAINT request_links_target_kind CHECK (target_kind IN (
            'project', 'epic', 'story', 'task', 'artifact', 'receipt', 'request')),
    target_id   INT NOT NULL
        CONSTRAINT request_links_target_id CHECK (target_id > 0),
    -- Who attached it, and the AMP user behind it. A connector or MCP service
    -- client has no AMP user, so this is nullable; deleting the user nulls the
    -- link and leaves the relation and its timestamp intact (the house rule for
    -- user references, as in 053's requester_user_id and 054's actor_user_id).
    created_by  INT REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- A request never links to itself: domain.RequestLink.Validate refuses it,
    -- and it is a one-row invariant, so the database can hold it as well. A
    -- self-link would make "what does this request relate to" report itself.
    CONSTRAINT request_links_no_self_link
        CHECK (target_kind <> 'request' OR target_id <> request_id),

    -- The kind and the target kind agree. This is the schema twin of
    -- domain.requestLinkTargets: routes_to is the one project a request is
    -- routed to (D9), planned_as the epics/stories/tasks its plan is made of,
    -- delivered_by the tasks/artifacts/receipts that prove delivery (D10), and
    -- duplicate_of/relates_to only ever point at another request. A planned_as
    -- link to an artifact, or a routes_to link to a ticket, describes a relation
    -- the product does not have.
    CONSTRAINT request_links_kind_target_pair CHECK (
        (link_kind = 'routes_to'    AND target_kind = 'project')
     OR (link_kind = 'planned_as'   AND target_kind IN ('epic', 'story', 'task'))
     OR (link_kind = 'delivered_by' AND target_kind IN ('task', 'artifact', 'receipt'))
     OR (link_kind = 'duplicate_of' AND target_kind = 'request')
     OR (link_kind = 'relates_to'   AND target_kind = 'request')),

    -- Tenant consistency is a constraint, not a convention in the write path:
    -- the request must be in the link's org (053's uq_team_requests_id_org gives
    -- this composite key its target). CASCADE is the retention rule — the link
    -- is deleted with the request it belongs to, never with its target.
    CONSTRAINT request_links_request_in_org
        FOREIGN KEY (request_id, org_id) REFERENCES team_requests (id, org_id)
        ON DELETE CASCADE
);

-- Dedup: the same relation attached twice is one row. This is the key a later
-- repository's `INSERT ... ON CONFLICT DO NOTHING` rides on (the
-- task_dependencies pattern), so a retried attach is a no-op rather than a
-- duplicate lineage row. created_by/created_at on the first row are kept
-- (first-write-wins, the merged_at rule).
CREATE UNIQUE INDEX IF NOT EXISTS uq_request_links_request_target
    ON request_links (request_id, target_kind, target_id, link_kind);

-- The plan rail: "this request's routes_to / planned_as / delivered_by rows", in
-- attach order. Ordered by id, not created_at, so rows written in one
-- transaction keep a stable order (the request_comments read rule).
CREATE INDEX IF NOT EXISTS idx_request_links_request
    ON request_links (request_id, link_kind, id);

-- The reverse read: "which requests does this ticket/artifact/receipt serve?" —
-- the lineage question a delivery projection or a trace asks, and the read the
-- unique key above cannot answer. It stays useful for a tombstone too: the row
-- still names its target, even when the target no longer exists to join to.
CREATE INDEX IF NOT EXISTS idx_request_links_target
    ON request_links (target_kind, target_id, link_kind);

-- Tenant-level reporting across a team's requests.
CREATE INDEX IF NOT EXISTS idx_request_links_org
    ON request_links (org_id, created_at DESC);
