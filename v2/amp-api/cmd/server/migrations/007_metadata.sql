-- Free-form key/value metadata on projects, epics, stories, and tasks, for
-- integrations and reporting (e.g. jira_board, initiative, capitalizable).
ALTER TABLE projects ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
ALTER TABLE epics    ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
ALTER TABLE stories  ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
ALTER TABLE tasks    ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
