-- Audit log v2, the SIEM sink (#2431, KB decisions/audit-log-v2.md section 4).
--
-- 1. Instance webhooks: a webhook with neither an org nor a project
--    belongs to the whole AMP. Only global admins manage them, and they
--    receive audit entries only, every one, including rows that belong to
--    no org (user.*, token.*, auth.*), which no org or project webhook can
--    receive.
-- 2. An optional static header sent with every delivery, for SIEM
--    collectors that authenticate with one (Splunk HEC "Authorization:
--    Splunk <token>", Datadog "DD-API-KEY: <key>"). The value is a secret,
--    kept like the signing secret: write-only through the API, never
--    returned, logged or put in an audit row.
--
-- Additive: existing webhooks keep their scope and get no header.

DO $$
DECLARE c record;
BEGIN
    -- 011 made the scope CHECK ((org_id IS NULL) <> (project_id IS NULL))
    -- without a name; find it by what it says.
    FOR c IN
        SELECT conname FROM pg_constraint
        WHERE conrelid = 'webhooks'::regclass AND contype = 'c'
          AND pg_get_constraintdef(oid) LIKE '%org_id IS NULL%<>%project_id IS NULL%'
    LOOP
        EXECUTE format('ALTER TABLE webhooks DROP CONSTRAINT %I', c.conname);
    END LOOP;
END $$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'webhooks'::regclass AND conname = 'webhooks_scope_check') THEN
        -- At most one of org and project; neither = the whole instance.
        ALTER TABLE webhooks ADD CONSTRAINT webhooks_scope_check CHECK (org_id IS NULL OR project_id IS NULL);
    END IF;
END $$;

ALTER TABLE webhooks
    ADD COLUMN IF NOT EXISTS auth_header_name  TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS auth_header_value TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_webhooks_instance ON webhooks(id) WHERE active AND org_id IS NULL AND project_id IS NULL;
