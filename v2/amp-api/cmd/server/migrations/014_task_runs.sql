-- Runs: one record per agent run on a ticket — the model it used, the
-- tokens it spent (when its harness can tell it), and where its work went
-- (branch, commit, PR). Reports total token spend from these; a commit or
-- branch leads back to the ticket and its history.
--
-- task_id has no foreign key on purpose: the record of what was spent and
-- shipped outlives the ticket if it is deleted.
CREATE TABLE IF NOT EXISTS task_runs (
    id                 BIGSERIAL PRIMARY KEY,
    task_id            INT NOT NULL,
    project_id         INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id            INT REFERENCES users(id) ON DELETE SET NULL,
    agent_id           TEXT NOT NULL DEFAULT '',
    outcome            TEXT NOT NULL DEFAULT '',  -- completed | blocked | progress | abandoned
    model              TEXT NOT NULL DEFAULT '',
    input_tokens       BIGINT,
    output_tokens      BIGINT,
    cache_read_tokens  BIGINT,
    cache_write_tokens BIGINT,
    cost_usd           NUMERIC(14, 6),
    branch             TEXT NOT NULL DEFAULT '',
    commit_sha         TEXT NOT NULL DEFAULT '',
    pr_url             TEXT NOT NULL DEFAULT '',
    pushed             BOOLEAN,
    note               TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_task_runs_task    ON task_runs(task_id);
CREATE INDEX IF NOT EXISTS idx_task_runs_project ON task_runs(project_id, created_at);
CREATE INDEX IF NOT EXISTS idx_task_runs_commit  ON task_runs(commit_sha) WHERE commit_sha <> '';
CREATE INDEX IF NOT EXISTS idx_task_runs_branch  ON task_runs(project_id, branch) WHERE branch <> '';
