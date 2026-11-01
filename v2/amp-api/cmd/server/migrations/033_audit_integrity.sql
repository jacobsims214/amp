-- Audit log v2 integrity (KB decisions/audit-log-v2.md section 4, ticket
-- #2429): append-only, tamper-evident, with a retention path that keeps the
-- chain verifiable, and a durable outbox for audit webhooks.
--
-- Numbered 033: 031 is #2427's context columns, 032 is #2428's coverage.
--
-- Role `amp` owns audit_log and is also the runtime role, so grants alone
-- can't make the table append-only: an owner can re-grant itself. The
-- defence is in layers:
--   1. audit_log_guard (below) refuses UPDATE, DELETE and TRUNCATE for every
--      role, owner and superuser included, with two exceptions: the
--      users(id) ON DELETE SET NULL action on actor_user_id, and deletes by
--      audit_purge() of rows an anchor checkpoint covers.
--   2. A per-chain hash chain computed by a BEFORE INSERT trigger, so every
--      writer is covered (amp-api, authadmin, and m1's relay_record_audit,
--      which inserts with an explicit column list) without changing them.
--   3. Checkpoints of each chain's head, signed by amp-api with an Ed25519
--      key that lives in a Kubernetes Secret, never in this database.
--      Someone who can disable the triggers can rewrite rows and re-hash the
--      chain, but can't forge a signature over the old head.
--   4. audit_outbox: every row is delivered to webhooks (the SIEM path) at
--      least once.

-- Nothing writes the table while it's chained, so the backfill below sees
-- every row (old replicas' inserts wait for this migration to commit, then
-- get chained by the trigger).
LOCK TABLE audit_log IN SHARE ROW EXCLUSIVE MODE;

ALTER TABLE audit_log
    -- org:<id>, or global for rows with no org
    ADD COLUMN IF NOT EXISTS chain_key TEXT,
    -- 1, 2, 3... within chain_key, gapless while nothing is purged
    ADD COLUMN IF NOT EXISTS chain_seq BIGINT,
    -- the previous row's hash (32 zero bytes for seq 1)
    ADD COLUMN IF NOT EXISTS prev_hash BYTEA,
    -- sha256(prev_hash || audit_canonical(row))
    ADD COLUMN IF NOT EXISTS hash      BYTEA;

CREATE UNIQUE INDEX IF NOT EXISTS idx_audit_chain ON audit_log(chain_key, chain_seq);

-- The newest seq and hash of each chain. The chain trigger locks a chain's
-- row, so inserts into one chain are serialised (fine at audit volume).
CREATE TABLE IF NOT EXISTS audit_chain_heads (
    chain_key  TEXT PRIMARY KEY,
    seq        BIGINT NOT NULL,
    hash       BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Signed statements "chain K's row upto_seq has hash H". periodic ones are
-- written hourly; an anchor is written before retention deletes the rows up
-- to upto_seq, and verification starts from it. signature is NULL when no
-- signing key is configured (key_id '' then).
CREATE TABLE IF NOT EXISTS audit_checkpoints (
    id        BIGSERIAL PRIMARY KEY,
    chain_key TEXT NOT NULL,
    upto_seq  BIGINT NOT NULL,
    hash      BYTEA NOT NULL,
    at        TIMESTAMPTZ NOT NULL,
    kind      TEXT NOT NULL CHECK (kind IN ('periodic', 'anchor')),
    key_id    TEXT NOT NULL DEFAULT '',
    signature BYTEA
);
CREATE INDEX IF NOT EXISTS idx_audit_checkpoints_chain ON audit_checkpoints(chain_key, upto_seq);

-- Every audit row, queued for the webhook dispatcher in the same statement
-- that wrote it. The dispatcher leases rows (next_attempt_at), matches them
-- to webhooks, and deletes them, so delivery is at least once. It replaces
-- the in-memory audit queue, which dropped entries when full.
CREATE TABLE IF NOT EXISTS audit_outbox (
    audit_id        BIGINT PRIMARY KEY REFERENCES audit_log(id) ON DELETE CASCADE,
    queued_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempts        INT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_audit_outbox_due ON audit_outbox(next_attempt_at, audit_id);

-- The text a row's hash covers: every column except actor_user_id (the
-- users FK sets it to NULL when a user is deleted; actor_email keeps who it
-- was) and the hashes themselves. jsonb's text form is deterministic (keys
-- ordered by length, then bytes); `at` is fixed to UTC with microseconds so
-- the session time zone doesn't matter. internal/auditchain.Canonical
-- builds the same text in Go, so verification doesn't trust this function.
-- A column added to audit_log later is not covered until this function and
-- auditchain.Canonical both add it.
CREATE OR REPLACE FUNCTION audit_canonical(r audit_log) RETURNS TEXT
LANGUAGE sql STABLE AS $$
    SELECT jsonb_build_object(
        'id',                   r.id,
        'at',                   to_char(r.at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
        'actor_email',          r.actor_email,
        'via_token',            r.via_token,
        'org_id',               r.org_id,
        'project_id',           r.project_id,
        'action',               r.action,
        'target_type',          r.target_type,
        'target_id',            r.target_id,
        'summary',              r.summary,
        'details',              r.details,
        'schema_version',       r.schema_version,
        'actor_type',           r.actor_type,
        'auth_method',          r.auth_method,
        'token_id',             r.token_id,
        'session_id',           r.session_id,
        'on_behalf_of_user_id', r.on_behalf_of_user_id,
        'agent_profile',        r.agent_profile,
        'target_name',          r.target_name,
        'outcome',              r.outcome,
        'reason',               r.reason,
        'source_ip',            r.source_ip::text,
        'user_agent',           r.user_agent,
        'client',               r.client,
        'request_id',           r.request_id,
        'trace_id',             r.trace_id,
        'before',               r."before",
        'after',                r."after",
        'chain_key',            r.chain_key,
        'chain_seq',            r.chain_seq
    )::text
$$;

CREATE OR REPLACE FUNCTION audit_link_hash(p_prev BYTEA, p_canonical TEXT) RETURNS BYTEA
LANGUAGE sql IMMUTABLE AS $$
    SELECT sha256(p_prev || convert_to(p_canonical, 'UTF8'))
$$;

CREATE OR REPLACE FUNCTION audit_chain_key(p_org_id INT) RETURNS TEXT
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE WHEN p_org_id IS NULL THEN 'global' ELSE 'org:' || p_org_id END
$$;

-- Chain the existing rows, in id order, before the guard exists.
DO $$
DECLARE
    r audit_log;
    k TEXT;
    s BIGINT;
    h BYTEA;
BEGIN
    FOR r IN SELECT * FROM audit_log WHERE hash IS NULL ORDER BY id LOOP
        k := audit_chain_key(r.org_id);
        INSERT INTO audit_chain_heads (chain_key, seq, hash) VALUES (k, 0, decode(repeat('00', 32), 'hex'))
            ON CONFLICT (chain_key) DO NOTHING;
        SELECT seq, hash INTO s, h FROM audit_chain_heads WHERE chain_key = k;
        r.chain_key := k;
        r.chain_seq := s + 1;
        r.prev_hash := h;
        r.hash := audit_link_hash(h, audit_canonical(r));
        UPDATE audit_log SET chain_key = r.chain_key, chain_seq = r.chain_seq, prev_hash = r.prev_hash, hash = r.hash
            WHERE id = r.id;
        UPDATE audit_chain_heads SET seq = r.chain_seq, hash = r.hash, updated_at = NOW() WHERE chain_key = k;
    END LOOP;
END $$;

-- BEFORE INSERT: whatever the writer sent, the chain columns are computed
-- here. SECURITY DEFINER so a role that may only INSERT into audit_log (or
-- call a SECURITY DEFINER writer, like m1's relay) needs no rights on the
-- heads or the outbox; the search_path is pinned to the one this migration
-- ran with.
CREATE OR REPLACE FUNCTION audit_log_chain() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$
DECLARE
    s BIGINT;
    h BYTEA;
BEGIN
    NEW.chain_key := audit_chain_key(NEW.org_id);
    INSERT INTO audit_chain_heads (chain_key, seq, hash) VALUES (NEW.chain_key, 0, decode(repeat('00', 32), 'hex'))
        ON CONFLICT (chain_key) DO NOTHING;
    SELECT seq, hash INTO s, h FROM audit_chain_heads WHERE chain_key = NEW.chain_key FOR UPDATE;
    NEW.chain_seq := s + 1;
    NEW.prev_hash := h;
    NEW.hash := audit_link_hash(h, audit_canonical(NEW));
    UPDATE audit_chain_heads SET seq = NEW.chain_seq, hash = NEW.hash, updated_at = NOW()
        WHERE chain_key = NEW.chain_key;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION audit_log_outbox() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$
BEGIN
    INSERT INTO audit_outbox (audit_id) VALUES (NEW.id);
    RETURN NULL;
END $$;

-- The guard. UPDATE is allowed only when it is the users FK's ON DELETE SET
-- NULL: actor_user_id goes to NULL, nothing else changes, and it runs inside
-- the FK's trigger (depth > 1), so a direct UPDATE can't use the exception.
-- DELETE is allowed only inside audit_purge(), and only for rows an anchor
-- checkpoint covers.
CREATE OR REPLACE FUNCTION audit_log_guard() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF pg_trigger_depth() > 1
           AND OLD.actor_user_id IS NOT NULL AND NEW.actor_user_id IS NULL
           AND (to_jsonb(NEW) - 'actor_user_id') = (to_jsonb(OLD) - 'actor_user_id') THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'audit_log is append-only: rows can''t be updated'
            USING ERRCODE = 'insufficient_privilege';
    ELSIF TG_OP = 'DELETE' THEN
        IF current_setting('amp.audit_purge', true) = 'on'
           AND OLD.chain_seq IS NOT NULL
           AND EXISTS (SELECT 1 FROM audit_checkpoints c WHERE c.chain_key = OLD.chain_key
                       AND c.kind = 'anchor' AND c.upto_seq >= OLD.chain_seq) THEN
            RETURN OLD;
        END IF;
        RAISE EXCEPTION 'audit_log is append-only: rows are only deleted by retention (audit_purge)'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RAISE EXCEPTION 'audit_log is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END $$;

-- Checkpoints are append-only too.
CREATE OR REPLACE FUNCTION audit_checkpoints_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_checkpoints is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END $$;

-- Retention's one way to delete audit rows: the rows of chain p_chain_key
-- up to p_upto_seq, once an anchor checkpoint at or past it exists. amp-api
-- writes (and signs) the anchor in the same transaction, just before.
CREATE OR REPLACE FUNCTION audit_purge(p_chain_key TEXT, p_upto_seq BIGINT) RETURNS BIGINT
LANGUAGE plpgsql AS $$
DECLARE
    n BIGINT;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM audit_checkpoints WHERE chain_key = p_chain_key AND kind = 'anchor'
                   AND upto_seq >= p_upto_seq) THEN
        RAISE EXCEPTION 'audit_purge: no anchor checkpoint covers %:%', p_chain_key, p_upto_seq;
    END IF;
    PERFORM set_config('amp.audit_purge', 'on', true);
    DELETE FROM audit_log WHERE chain_key = p_chain_key AND chain_seq <= p_upto_seq;
    GET DIAGNOSTICS n = ROW_COUNT;
    PERFORM set_config('amp.audit_purge', 'off', true);
    RETURN n;
END $$;

DROP TRIGGER IF EXISTS audit_log_chain ON audit_log;
CREATE TRIGGER audit_log_chain BEFORE INSERT ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_chain();

DROP TRIGGER IF EXISTS audit_log_outbox ON audit_log;
CREATE TRIGGER audit_log_outbox AFTER INSERT ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_outbox();

DROP TRIGGER IF EXISTS audit_log_guard ON audit_log;
CREATE TRIGGER audit_log_guard BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_guard();

DROP TRIGGER IF EXISTS audit_log_guard_truncate ON audit_log;
CREATE TRIGGER audit_log_guard_truncate BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_guard();

DROP TRIGGER IF EXISTS audit_checkpoints_guard ON audit_checkpoints;
CREATE TRIGGER audit_checkpoints_guard BEFORE UPDATE OR DELETE ON audit_checkpoints
    FOR EACH ROW EXECUTE FUNCTION audit_checkpoints_guard();

DROP TRIGGER IF EXISTS audit_checkpoints_guard_truncate ON audit_checkpoints;
CREATE TRIGGER audit_checkpoints_guard_truncate BEFORE TRUNCATE ON audit_checkpoints
    FOR EACH STATEMENT EXECUTE FUNCTION audit_checkpoints_guard();

-- Other roles never get to change history. (Prod's only grantee is the
-- owner, whom the guard covers; this closes PUBLIC for any future role.)
REVOKE UPDATE, DELETE, TRUNCATE ON audit_log, audit_checkpoints FROM PUBLIC;
