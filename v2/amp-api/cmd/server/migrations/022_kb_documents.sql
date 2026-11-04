-- KB documents in Postgres: the durable source of truth for project, team and
-- org KB docs (decisions/kb-postgres-source-of-truth.md). Typesense becomes a
-- search index rebuilt from these rows. The global KB (the AMP guide) stays
-- sourced from the repo's v2/kb folder and is not stored here.
--
-- scope_id is the project, team or org id. There is deliberately no foreign
-- key: deleting a team or org must not cascade away its docs. Removing a KB is
-- a soft delete (deleted_at) that a purge acts on later, like tickets' trash.

CREATE TABLE IF NOT EXISTS kb_documents (
    id          BIGSERIAL PRIMARY KEY,
    scope_kind  TEXT NOT NULL CHECK (scope_kind IN ('project', 'team', 'org')),
    scope_id    INT NOT NULL,
    path        TEXT NOT NULL CHECK (path <> ''),
    title       TEXT NOT NULL DEFAULT '',
    tags        TEXT[] NOT NULL DEFAULT '{}',
    author      TEXT NOT NULL DEFAULT '',
    -- Why a doc was placed in a team or org KB rather than a project's.
    reason      TEXT NOT NULL DEFAULT '',
    content     TEXT NOT NULL DEFAULT '',
    annotations JSONB NOT NULL DEFAULT '[]',
    -- The latest kb_document_versions.version for this doc; also lets
    -- writers detect a concurrent change (compare-and-set on version).
    version     INT NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Soft delete. Writing a deleted path again revives the same row, so a
    -- doc's history stays in one place.
    deleted_at  TIMESTAMPTZ,
    deleted_by  TEXT,
    UNIQUE (scope_kind, scope_id, path)
);

-- Listing a KB, newest first (live docs only).
CREATE INDEX IF NOT EXISTS idx_kb_documents_scope_updated
    ON kb_documents (scope_kind, scope_id, updated_at DESC)
    WHERE deleted_at IS NULL;

-- Finding soft-deleted docs for restore or purge.
CREATE INDEX IF NOT EXISTS idx_kb_documents_deleted
    ON kb_documents (deleted_at)
    WHERE deleted_at IS NOT NULL;

-- Append-only history: one row per change to a doc's content, title or tags
-- (and per delete/restore), with who made it and when. Annotation changes
-- are not versioned. Rows are never updated; they go only when their
-- document is purged.
CREATE TABLE IF NOT EXISTS kb_document_versions (
    id          BIGSERIAL PRIMARY KEY,
    document_id BIGINT NOT NULL REFERENCES kb_documents(id) ON DELETE CASCADE,
    version     INT NOT NULL,
    change      TEXT NOT NULL DEFAULT 'update'
                CHECK (change IN ('create', 'update', 'delete', 'restore', 'backfill')),
    title       TEXT NOT NULL DEFAULT '',
    tags        TEXT[] NOT NULL DEFAULT '{}',
    content     TEXT NOT NULL DEFAULT '',
    changed_by  TEXT NOT NULL DEFAULT '',
    changed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (document_id, version)
);

-- Per-KB backfill from Typesense into kb_documents. A row with completed_at
-- set means that KB's docs have all been copied; until then Typesense still
-- holds docs Postgres lacks, so nothing may treat Postgres as complete for it.
CREATE TABLE IF NOT EXISTS kb_backfills (
    scope_kind   TEXT NOT NULL CHECK (scope_kind IN ('project', 'team', 'org')),
    scope_id     INT NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    doc_count    INT NOT NULL DEFAULT 0,
    last_error   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (scope_kind, scope_id)
);
