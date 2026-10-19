-- Design systems: one per org (built in the org's Design System project and
-- published to the org KB) and optionally one per product project (built in
-- that project, published to its KB, extending the org's). Each publish is a
-- version; the docs under design-system/ are only written by a publish, from
-- phases a person approved.
CREATE TABLE IF NOT EXISTS design_systems (
    id                SERIAL PRIMARY KEY,
    scope             TEXT NOT NULL CHECK (scope IN ('org', 'project')),
    org_id            INT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    project_id        INT REFERENCES projects(id) ON DELETE CASCADE, -- the product project (scope project)
    source_project_id INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE, -- where the work happens
    version           INT NOT NULL DEFAULT 0,
    current_epic_id   INT REFERENCES epics(id) ON DELETE SET NULL,
    published_at      TIMESTAMPTZ,
    published_by      INT REFERENCES users(id) ON DELETE SET NULL,
    created_by        INT REFERENCES users(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_design_systems_org ON design_systems(org_id) WHERE scope = 'org';
CREATE UNIQUE INDEX IF NOT EXISTS uq_design_systems_project ON design_systems(project_id) WHERE scope = 'project';

CREATE TABLE IF NOT EXISTS design_system_versions (
    design_system_id INT NOT NULL REFERENCES design_systems(id) ON DELETE CASCADE,
    version          INT NOT NULL,
    changelog        TEXT NOT NULL DEFAULT '',
    artifacts        JSONB NOT NULL DEFAULT '[]',
    published_by     INT REFERENCES users(id) ON DELETE SET NULL,
    published_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (design_system_id, version)
);
