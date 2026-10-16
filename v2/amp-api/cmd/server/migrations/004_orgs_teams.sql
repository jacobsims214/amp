-- AMP v2 schema — orgs, teams, project access, and ownership.
--
-- Additive only: new tables, and nullable columns on existing ones. Runs
-- exactly once (tracked in schema_migrations), so the backfill at the end
-- won't re-link the default team to projects someone later removed it from.

CREATE TABLE IF NOT EXISTS orgs (
    id         SERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL UNIQUE,
    created_by INT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS org_members (
    org_id     INT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id    INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (org_id, user_id)
);

CREATE TABLE IF NOT EXISTS teams (
    id         SERIAL PRIMARY KEY,
    org_id     INT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_by INT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, name)
);

CREATE TABLE IF NOT EXISTS team_members (
    team_id    INT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id    INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('lead', 'member')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (team_id, user_id)
);

-- A team's role on a project: owner teams manage the project, member teams work in it.
CREATE TABLE IF NOT EXISTS project_teams (
    project_id INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    team_id    INT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('owner', 'member')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, team_id)
);

-- Pending memberships for people who haven't signed in yet. Applied on their
-- first login (matched on lower(email)), then marked accepted.
CREATE TABLE IF NOT EXISTS invites (
    id               SERIAL PRIMARY KEY,
    email            TEXT NOT NULL,
    org_id           INT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    org_role         TEXT NOT NULL DEFAULT 'member' CHECK (org_role IN ('owner', 'admin', 'member')),
    team_id          INT REFERENCES teams(id) ON DELETE CASCADE,
    team_role        TEXT NOT NULL DEFAULT 'member' CHECK (team_role IN ('lead', 'member')),
    invited_by       INT REFERENCES users(id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    accepted_at      TIMESTAMPTZ,
    accepted_user_id INT REFERENCES users(id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_invites_pending
    ON invites (lower(email), org_id, COALESCE(team_id, 0)) WHERE accepted_at IS NULL;

ALTER TABLE projects     ADD COLUMN IF NOT EXISTS org_id     INT REFERENCES orgs(id) ON DELETE RESTRICT;
ALTER TABLE projects     ADD COLUMN IF NOT EXISTS created_by INT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE epics        ADD COLUMN IF NOT EXISTS created_by INT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE stories      ADD COLUMN IF NOT EXISTS created_by INT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE tasks        ADD COLUMN IF NOT EXISTS created_by INT REFERENCES users(id) ON DELETE SET NULL;
-- At most one owner per ticket, by construction. NULL = unassigned.
ALTER TABLE tasks        ADD COLUMN IF NOT EXISTS owner_user_id INT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE comments     ADD COLUMN IF NOT EXISTS author_user_id INT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE activity_log ADD COLUMN IF NOT EXISTS actor_user_id  INT REFERENCES users(id) ON DELETE SET NULL;

-- Epics and stories are shared groupings that can have many owners.
CREATE TABLE IF NOT EXISTS epic_owners (
    epic_id INT NOT NULL REFERENCES epics(id) ON DELETE CASCADE,
    user_id INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (epic_id, user_id)
);
CREATE TABLE IF NOT EXISTS story_owners (
    story_id INT NOT NULL REFERENCES stories(id) ON DELETE CASCADE,
    user_id  INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (story_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_org_members_user   ON org_members(user_id);
CREATE INDEX IF NOT EXISTS idx_team_members_user  ON team_members(user_id);
CREATE INDEX IF NOT EXISTS idx_teams_org          ON teams(org_id);
CREATE INDEX IF NOT EXISTS idx_project_teams_team ON project_teams(team_id);
CREATE INDEX IF NOT EXISTS idx_projects_org       ON projects(org_id);
CREATE INDEX IF NOT EXISTS idx_tasks_owner        ON tasks(owner_user_id);
CREATE INDEX IF NOT EXISTS idx_tasks_created_by   ON tasks(created_by);
CREATE INDEX IF NOT EXISTS idx_epic_owners_user   ON epic_owners(user_id);
CREATE INDEX IF NOT EXISTS idx_story_owners_user  ON story_owners(user_id);

-- ---- One-time backfill: nobody loses access on upgrade ----
-- Every existing user joins a "Default" org and its "Everyone" team; global
-- admins become org owners and team leads. "Everyone" owns every existing
-- project. Fresh installs get the same empty org + team to start from.
INSERT INTO orgs (name, slug) VALUES ('Default', 'default') ON CONFLICT (slug) DO NOTHING;

INSERT INTO teams (org_id, name)
SELECT id, 'Everyone' FROM orgs WHERE slug = 'default'
ON CONFLICT (org_id, name) DO NOTHING;

INSERT INTO org_members (org_id, user_id, role)
SELECT o.id, u.id,
       CASE WHEN EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id = u.id AND ur.role = 'admin')
            THEN 'owner' ELSE 'member' END
FROM users u CROSS JOIN orgs o WHERE o.slug = 'default'
ON CONFLICT DO NOTHING;

INSERT INTO team_members (team_id, user_id, role)
SELECT t.id, u.id,
       CASE WHEN EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id = u.id AND ur.role = 'admin')
            THEN 'lead' ELSE 'member' END
FROM users u
JOIN teams t ON t.name = 'Everyone'
JOIN orgs o ON o.id = t.org_id AND o.slug = 'default'
ON CONFLICT DO NOTHING;

UPDATE projects SET org_id = (SELECT id FROM orgs WHERE slug = 'default') WHERE org_id IS NULL;

INSERT INTO project_teams (project_id, team_id, role)
SELECT p.id, t.id, 'owner'
FROM projects p
JOIN teams t ON t.name = 'Everyone'
JOIN orgs o ON o.id = t.org_id AND o.slug = 'default'
ON CONFLICT DO NOTHING;
