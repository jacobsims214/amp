-- Artifacts: things agents make for people to look at before (or instead of)
-- code — mockups, prototypes, diagrams, images, documents. Each belongs to a
-- project and usually a ticket, is versioned, and a person reviews it:
-- approve, or ask for changes. AMP serves them sandboxed, so any harness can
-- publish one and everyone on the project sees the same thing.
CREATE TABLE IF NOT EXISTS artifacts (
    id               SERIAL PRIMARY KEY,
    project_id       INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id          INT REFERENCES tasks(id) ON DELETE SET NULL,
    title            TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('html', 'svg', 'image', 'markdown', 'pdf', 'text')),
    status           TEXT NOT NULL DEFAULT 'in_review'
                     CHECK (status IN ('in_review', 'approved', 'changes_requested')),
    current_version  INT NOT NULL DEFAULT 1,
    approved_version INT,
    reviewed_by      INT REFERENCES users(id) ON DELETE SET NULL,
    reviewed_at      TIMESTAMPTZ,
    created_by       INT REFERENCES users(id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_artifacts_project ON artifacts(project_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_artifacts_task ON artifacts(task_id) WHERE task_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS artifact_versions (
    artifact_id  INT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    version      INT NOT NULL,
    content      BYTEA NOT NULL,
    content_type TEXT NOT NULL,
    size_bytes   INT NOT NULL,
    sha256       TEXT NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    created_by   INT REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (artifact_id, version)
);
