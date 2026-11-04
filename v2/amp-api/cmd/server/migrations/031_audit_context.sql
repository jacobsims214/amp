-- Audit log v2 (KB decisions/audit-log-v2.md, ticket #2427): who, from
-- where, how and with what result each audited change was made.
-- Additive only: every new column is nullable or defaulted, so rows written
-- before this read as schema_version 1 with empty context, and older
-- amp-api replicas (and m1's relay_record_audit, which inserts with an
-- explicit column list) keep inserting unchanged during a rolling deploy.
-- Numbered 031 so it never collides with remote-m1's 027-030; the runner
-- applies each missing version by name, so the order of arrival is fine.
-- The integrity columns (chain_key, chain_seq, prev_hash, hash) come with
-- #2429's migration.
ALTER TABLE audit_log
    ADD COLUMN IF NOT EXISTS schema_version       SMALLINT NOT NULL DEFAULT 1,
    -- human, agent, system or anonymous
    ADD COLUMN IF NOT EXISTS actor_type           TEXT NOT NULL DEFAULT '',
    -- dex_session, oauth_mcp, api_token, cluster, local_dev or none
    ADD COLUMN IF NOT EXISTS auth_method          TEXT NOT NULL DEFAULT '',
    -- api_tokens.id when auth_method = api_token; never the token
    ADD COLUMN IF NOT EXISTS token_id             BIGINT,
    -- truncated SHA-256 of the Dex token's sub and iat; never the token
    ADD COLUMN IF NOT EXISTS session_id           TEXT,
    ADD COLUMN IF NOT EXISTS on_behalf_of_user_id INT,
    ADD COLUMN IF NOT EXISTS agent_profile        TEXT,
    ADD COLUMN IF NOT EXISTS target_name          TEXT,
    -- success, denied or failed
    ADD COLUMN IF NOT EXISTS outcome              TEXT NOT NULL DEFAULT 'success',
    ADD COLUMN IF NOT EXISTS reason               TEXT NOT NULL DEFAULT '',
    -- the trusted client address (reqctx); NULL for system jobs, with
    -- details.ip_reason saying why
    ADD COLUMN IF NOT EXISTS source_ip            INET,
    ADD COLUMN IF NOT EXISTS user_agent           TEXT,
    -- web, mcp, harness, api, relay, cli or system
    ADD COLUMN IF NOT EXISTS client               TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS request_id           TEXT,
    ADD COLUMN IF NOT EXISTS trace_id             TEXT,
    ADD COLUMN IF NOT EXISTS "before"             JSONB,
    ADD COLUMN IF NOT EXISTS "after"              JSONB;

CREATE INDEX IF NOT EXISTS idx_audit_source_ip ON audit_log(source_ip, id DESC) WHERE source_ip IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_audit_outcome ON audit_log(outcome, id DESC) WHERE outcome <> 'success';
CREATE INDEX IF NOT EXISTS idx_audit_target ON audit_log(target_type, target_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_at_brin ON audit_log USING BRIN (at);
