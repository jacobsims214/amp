-- Deleted tickets go to a trash for 30 days: a snapshot of the ticket, its
-- comments, activity, and dependency links, so a deletion (or a plan wiped by
-- mistake) can be undone with amp_restore_tasks or from the UI.
CREATE TABLE IF NOT EXISTS deleted_tasks (
    task_id    INT PRIMARY KEY,
    project_id INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    epic_id    INT NOT NULL,
    story_id   INT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    snapshot   JSONB NOT NULL,
    deleted_by INT REFERENCES users(id) ON DELETE SET NULL,
    deleted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_deleted_tasks_project ON deleted_tasks(project_id, deleted_at);
