-- Deleted epics and stories go to the trash too (their tickets already go to
-- deleted_tasks): a snapshot of the epic or story rows and their owners, and
-- the tickets deleted with them, so the whole thing can be restored.
CREATE TABLE IF NOT EXISTS deleted_groups (
    kind       TEXT NOT NULL CHECK (kind IN ('epic', 'story')),
    item_id    INT NOT NULL,
    project_id INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    epic_id    INT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    snapshot   JSONB NOT NULL,
    task_ids   INT[] NOT NULL DEFAULT '{}',
    deleted_by INT REFERENCES users(id) ON DELETE SET NULL,
    deleted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (kind, item_id)
);
CREATE INDEX IF NOT EXISTS idx_deleted_groups_project ON deleted_groups(project_id, deleted_at);
