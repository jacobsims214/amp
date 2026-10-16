-- Daily reports: one per user per day — what they got done, what's in
-- flight and blocked, and their own summary — kept as a record and for
-- velocity, readable by everyone in the author's orgs.
CREATE TABLE IF NOT EXISTS day_reports (
    id         BIGSERIAL PRIMARY KEY,
    user_id    INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day        DATE NOT NULL,
    tz         TEXT NOT NULL,
    summary    TEXT NOT NULL DEFAULT '',
    data       JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, day)
);
CREATE INDEX IF NOT EXISTS idx_day_reports_day ON day_reports(day);

-- Reports and velocity look up a user's activity and completions by time.
CREATE INDEX IF NOT EXISTS idx_activity_actor_user_created ON activity_log(actor_user_id, created_at);
CREATE INDEX IF NOT EXISTS idx_tasks_owner_completed ON tasks(owner_user_id, completed_at) WHERE completed_at IS NOT NULL;
