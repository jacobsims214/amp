-- Audit log integrity, security review fixes (#2432 review, ticket #2439).
-- Numbered 035: 034 is #2431's webhook columns (feat/audit-ui).
--
-- H1. Retention's delete path could be reached by any session of the app
-- role: set amp.audit_purge=on, insert an anchor checkpoint, delete. Now:
--   * Only audit_purge() deletes rows, and it is SECURITY DEFINER owned by
--     a separate role, amp_audit_purge. The guard allows a DELETE only when
--     it runs as that role (current_user), not because of a session flag.
--   * Anchor checkpoints can only be written by audit_purge(), which takes
--     the anchored row's hash and time from the row itself, and refuses rows
--     younger than 30 days. A caller can still pass any signature bytes (the
--     database can't check Ed25519), so `audit verify` is what proves an
--     anchor: it requires a valid signature whenever a key is configured.
--   * The role needs a superuser to set up (CREATE ROLE, and handing the
--     function to it). The block marked "audit_purge role setup" below does
--     it when this migration runs as a superuser (local compose, CI). Where
--     it doesn't (CloudNativePG: the app role is not a superuser), retention
--     refuses to delete anything until a superuser runs that block, which
--     `amp-api audit purge-role-sql` prints. Until then nothing is purged.
--   * Role `amp` owns audit_log, so it can still disable or drop the guard
--     trigger. That is tampering the signed checkpoints catch; this closes
--     the path that needed no DDL at all.
--
-- L1. actor_user_id stays out of the hash (the users FK sets it to NULL
-- when a user is deleted). actor_id_at_write, set by the chain trigger from
-- actor_user_id, has no FK and is hashed, so re-pointing actor_user_id to
-- another user shows up in verify. Rows from before this migration keep
-- their old canonical text (the key is left out while the column is NULL).
--
-- M2. webhooks.include_context: org and project webhooks receive the v2
-- request context (source_ip, user_agent, session_id, token_id, before,
-- after) only when they opt in. Off for every existing webhook.

ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS actor_id_at_write INT;
ALTER TABLE audit_checkpoints ADD COLUMN IF NOT EXISTS row_at TIMESTAMPTZ;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS include_context BOOLEAN NOT NULL DEFAULT false;

-- Canonical text: as in 033, plus actor_id_at_write when it is set. Must
-- match internal/auditchain.Canonical.
CREATE OR REPLACE FUNCTION audit_canonical(r audit_log) RETURNS TEXT
LANGUAGE sql STABLE AS $$
    SELECT (jsonb_build_object(
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
    ) || CASE WHEN r.actor_id_at_write IS NULL THEN '{}'::jsonb
              ELSE jsonb_build_object('actor_id_at_write', r.actor_id_at_write) END)::text
$$;

-- The chain trigger now also records who wrote the row, whatever the
-- writer sent for actor_id_at_write.
CREATE OR REPLACE FUNCTION audit_log_chain() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$
DECLARE
    s BIGINT;
    h BYTEA;
BEGIN
    NEW.actor_id_at_write := NEW.actor_user_id;
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

-- The guard. No longer SECURITY DEFINER: it must see the role the DELETE
-- runs as. UPDATE: only the users FK's SET NULL (unchanged). DELETE: only
-- as amp_audit_purge (inside audit_purge()), and only rows an anchor
-- covers. The amp.audit_purge session flag no longer means anything.
CREATE OR REPLACE FUNCTION audit_log_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
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
        IF current_user = 'amp_audit_purge'
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

-- Anchors are written only by audit_purge() (as amp_audit_purge).
CREATE OR REPLACE FUNCTION audit_checkpoints_insert_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.kind = 'anchor' AND current_user <> 'amp_audit_purge' THEN
        RAISE EXCEPTION 'audit_checkpoints: anchors are written only by audit_purge()'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS audit_checkpoints_insert_guard ON audit_checkpoints;
CREATE TRIGGER audit_checkpoints_insert_guard BEFORE INSERT ON audit_checkpoints
    FOR EACH ROW EXECUTE FUNCTION audit_checkpoints_insert_guard();

-- Retention's one way to delete audit rows: write the anchor at
-- p_chain_key:p_upto_seq (hash and row time from the row; at, key_id and
-- signature from the caller, which signed it), then delete the chain's
-- rows up to it. Rows younger than 30 days are never deleted. Returns the
-- anchor's id, how many rows went, and the anchored row's time.
DROP FUNCTION IF EXISTS audit_purge(TEXT, BIGINT);
CREATE OR REPLACE FUNCTION audit_purge(p_chain_key TEXT, p_upto_seq BIGINT, p_hash BYTEA, p_at TIMESTAMPTZ,
                                       p_key_id TEXT, p_signature BYTEA)
RETURNS TABLE (anchor_id BIGINT, deleted BIGINT, row_at TIMESTAMPTZ)
LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$
DECLARE
    h   BYTEA;
    rat TIMESTAMPTZ;
    aid BIGINT;
    n   BIGINT;
BEGIN
    IF current_user <> 'amp_audit_purge' THEN
        RAISE EXCEPTION 'audit retention is off until a superuser sets up the amp_audit_purge role (amp-api audit purge-role-sql)'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    SELECT l.hash, l.at INTO h, rat FROM audit_log l WHERE l.chain_key = p_chain_key AND l.chain_seq = p_upto_seq;
    IF h IS NULL OR h <> p_hash THEN
        RAISE EXCEPTION 'audit_purge: %:% is not the row that was signed', p_chain_key, p_upto_seq;
    END IF;
    IF rat > NOW() - INTERVAL '30 days' THEN
        RAISE EXCEPTION 'audit_purge: %:% is younger than 30 days', p_chain_key, p_upto_seq
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    INSERT INTO audit_checkpoints (chain_key, upto_seq, hash, at, kind, key_id, signature, row_at)
        VALUES (p_chain_key, p_upto_seq, h, p_at, 'anchor', COALESCE(p_key_id, ''), p_signature, rat)
        RETURNING id INTO aid;
    DELETE FROM audit_log WHERE chain_key = p_chain_key AND chain_seq <= p_upto_seq;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN QUERY SELECT aid, n, rat;
END $$;

-- BEGIN audit_purge role setup
-- Run as a superuser, connected to AMP's database. Idempotent. Gives
-- audit_purge() to a NOLOGIN role that only it acts as; the app role keeps
-- EXECUTE on it. Skipped (with a notice) when not run by a superuser.
DO $setup$
DECLARE
    app TEXT;
BEGIN
    IF NOT (SELECT rolsuper FROM pg_roles WHERE rolname = current_user) THEN
        RAISE NOTICE 'amp_audit_purge: not a superuser, skipping; audit retention stays off until a superuser runs `amp-api audit purge-role-sql`';
        RETURN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'amp_audit_purge') THEN
        BEGIN
            CREATE ROLE amp_audit_purge NOLOGIN;
        EXCEPTION WHEN duplicate_object OR unique_violation THEN
            NULL; -- another database of this cluster made it meanwhile
        END;
    END IF;
    SELECT c.relowner::regrole::text INTO app FROM pg_class c WHERE c.oid = 'audit_log'::regclass;
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO amp_audit_purge',
                   (SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = 'audit_log'::regclass));
    GRANT SELECT, DELETE ON audit_log TO amp_audit_purge;
    GRANT SELECT, INSERT ON audit_checkpoints TO amp_audit_purge;
    GRANT USAGE ON SEQUENCE audit_checkpoints_id_seq TO amp_audit_purge;
    ALTER FUNCTION audit_purge(TEXT, BIGINT, BYTEA, TIMESTAMPTZ, TEXT, BYTEA) OWNER TO amp_audit_purge;
    REVOKE ALL ON FUNCTION audit_purge(TEXT, BIGINT, BYTEA, TIMESTAMPTZ, TEXT, BYTEA) FROM PUBLIC;
    EXECUTE format('GRANT EXECUTE ON FUNCTION audit_purge(TEXT, BIGINT, BYTEA, TIMESTAMPTZ, TEXT, BYTEA) TO %s', app);
END $setup$;
-- END audit_purge role setup
