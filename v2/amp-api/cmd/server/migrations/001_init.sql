-- AMP v2 schema
-- Run once on first startup. Safe to re-run (all CREATE IF NOT EXISTS).

CREATE TABLE IF NOT EXISTS projects (
    id          SERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    code        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT 'active',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS epics (
    id          SERIAL PRIMARY KEY,
    project_id  INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT 'backlog',
    priority    TEXT NOT NULL DEFAULT '1',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS stories (
    id                  SERIAL PRIMARY KEY,
    project_id          INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    epic_id             INT NOT NULL REFERENCES epics(id) ON DELETE CASCADE,
    name                TEXT NOT NULL,
    description         TEXT NOT NULL DEFAULT '',
    acceptance_criteria TEXT NOT NULL DEFAULT '',
    state               TEXT NOT NULL DEFAULT 'backlog',
    priority            TEXT NOT NULL DEFAULT '1',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS tasks (
    id                  SERIAL PRIMARY KEY,
    project_id          INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    epic_id             INT NOT NULL REFERENCES epics(id) ON DELETE CASCADE,
    story_id            INT NOT NULL REFERENCES stories(id) ON DELETE CASCADE,
    name                TEXT NOT NULL,
    description         TEXT NOT NULL DEFAULT '',
    acceptance_criteria TEXT NOT NULL DEFAULT '',
    state               TEXT NOT NULL DEFAULT 'backlog',
    priority            TEXT NOT NULL DEFAULT '1',
    assigned_to         TEXT NOT NULL DEFAULT '', -- set at plan time by manager
    agent_id            TEXT NOT NULL DEFAULT '', -- set at dispatch time by actor
    dispatched_at       TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    block_reason        TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Task dependencies (many-to-many: a task can depend on multiple tasks)
CREATE TABLE IF NOT EXISTS task_dependencies (
    task_id    INT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    depends_on INT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    PRIMARY KEY (task_id, depends_on)
);

CREATE TABLE IF NOT EXISTS comments (
    id         SERIAL PRIMARY KEY,
    task_id    INT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    body       TEXT NOT NULL,
    author     TEXT NOT NULL DEFAULT 'system',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Activity log — immutable audit trail.
-- Every state transition and comment produces one row. Never updated, only appended.
CREATE TABLE IF NOT EXISTS activity_log (
    id         SERIAL PRIMARY KEY,
    task_id    INT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    project_id INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    actor      TEXT NOT NULL DEFAULT 'system', -- who did it: agent ID, "manager", "system"
    action     TEXT NOT NULL,                  -- "created" "dispatched" "completed" "blocked" "comment" "state_change"
    from_state TEXT NOT NULL DEFAULT '',
    to_state   TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes for common query patterns
CREATE INDEX IF NOT EXISTS idx_tasks_project_id ON tasks(project_id);
CREATE INDEX IF NOT EXISTS idx_tasks_state      ON tasks(state);
CREATE INDEX IF NOT EXISTS idx_tasks_story_id   ON tasks(story_id);
CREATE INDEX IF NOT EXISTS idx_tasks_epic_id    ON tasks(epic_id);
CREATE INDEX IF NOT EXISTS idx_comments_task_id    ON comments(task_id);
CREATE INDEX IF NOT EXISTS idx_activity_task_id    ON activity_log(task_id);
CREATE INDEX IF NOT EXISTS idx_activity_project_id ON activity_log(project_id);
CREATE INDEX IF NOT EXISTS idx_epics_project_id ON epics(project_id);
CREATE INDEX IF NOT EXISTS idx_stories_epic_id  ON stories(epic_id);
