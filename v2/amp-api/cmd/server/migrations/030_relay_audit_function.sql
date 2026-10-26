-- amp-relay's audit entries go through a function, not table grants (AMP
-- #2399, re-review #2394 N3); refresh-proof nonces are shared by every relay
-- replica (N6).
--
-- Before this, the relay's role could INSERT any row into audit_log and any
-- body into webhook_deliveries. amp-api's delivery loop HMAC-signs and POSTs
-- whatever body is in the outbox, so a compromised relay could make AMP send
-- signed webhooks with any content to any org (a fake task.completed, say).
-- Now the relay calls relay_record_audit: a SECURITY DEFINER function (owned
-- by amp-api's role, which runs this migration) that accepts only the
-- remote.* actions the relay records, about a session that exists and that
-- the actor owns, with a fixed set of scalar detail members. The function
-- fills the actor's email, the org, the project, the target and the summary
-- itself, writes the audit row and queues its id in relay_audit_outbox.
-- amp-api's webhook dispatcher takes ids from that queue and renders and
-- signs the deliveries from the stored row, as it does for its own entries.
-- GrantRelayRole revokes the relay's INSERT on audit_log and
-- webhook_deliveries and grants EXECUTE on this function only.
--
-- remote_proof_nonces: the token-refresh proof's nonces (protocol 8.2), kept
-- for twice the ts window. A per-replica cache let a captured proof be
-- replayed once on each other replica; the primary key makes the check
-- cluster-wide.
--
-- Additive and idempotent.

CREATE TABLE IF NOT EXISTS relay_audit_outbox (
    audit_id   BIGINT PRIMARY KEY REFERENCES audit_log(id) ON DELETE CASCADE,
    queued_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS remote_proof_nonces (
    session_id TEXT NOT NULL REFERENCES remote_sessions(id) ON DELETE CASCADE,
    nonce      TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (session_id, nonce)
);
CREATE INDEX IF NOT EXISTS remote_proof_nonces_expires ON remote_proof_nonces (expires_at);

CREATE OR REPLACE FUNCTION relay_record_audit(
    p_actor_user_id INT,
    p_session_id    TEXT,
    p_action        TEXT,
    p_details       JSONB
) RETURNS TABLE (audit_id BIGINT, audit_at TIMESTAMPTZ)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
DECLARE
    v_email   TEXT := '';
    v_owner   INT;
    v_project INT;
    v_org     INT;
    v_host    TEXT := '';
    v_harness TEXT;
    v_details JSONB := '{}'::jsonb;
    v_key     TEXT;
    v_val     JSONB;
    v_summary TEXT;
    v_id      BIGINT;
    v_at      TIMESTAMPTZ;
BEGIN
    -- Only what the relay records (amp-api records remote.policy.set and
    -- remote.kill itself).
    IF p_action IS NULL OR p_action NOT IN ('remote.enable', 'remote.disable', 'remote.expire', 'remote.pair',
            'remote.pair.reject', 'remote.device.revoke', 'remote.attach', 'remote.detach', 'remote.prompt',
            'remote.stop', 'remote.refused') THEN
        RAISE EXCEPTION 'relay_record_audit: action % is not one the relay records', p_action
            USING ERRCODE = '22023';
    END IF;

    IF p_actor_user_id IS NOT NULL THEN
        SELECT u.email INTO v_email FROM users u WHERE u.id = p_actor_user_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'relay_record_audit: no user %', p_actor_user_id USING ERRCODE = '22023';
        END IF;
    ELSIF p_action NOT IN ('remote.expire', 'remote.disable') THEN
        -- Only the relay's own sweeps end sessions without a person.
        RAISE EXCEPTION 'relay_record_audit: % needs an actor', p_action USING ERRCODE = '22023';
    END IF;

    IF p_session_id IS NOT NULL THEN
        SELECT s.owner_user_id, s.project_id, s.host, s.harness_id
          INTO v_owner, v_project, v_host, v_harness
          FROM remote_sessions s WHERE s.id = p_session_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'relay_record_audit: no session %', p_session_id USING ERRCODE = '22023';
        END IF;
        IF p_actor_user_id IS NOT NULL AND p_actor_user_id <> v_owner THEN
            RAISE EXCEPTION 'relay_record_audit: the actor does not own session %', p_session_id
                USING ERRCODE = '22023';
        END IF;
        SELECT p.org_id INTO v_org FROM projects p WHERE p.id = v_project;
    ELSIF p_action <> 'remote.device.revoke' THEN
        RAISE EXCEPTION 'relay_record_audit: % needs a session', p_action USING ERRCODE = '22023';
    END IF;

    -- Details: known members only, each a short scalar. The session's own
    -- members come from the session row, not the caller.
    IF p_details IS NOT NULL THEN
        IF jsonb_typeof(p_details) <> 'object' THEN
            RAISE EXCEPTION 'relay_record_audit: details must be an object' USING ERRCODE = '22023';
        END IF;
        FOR v_key, v_val IN SELECT * FROM jsonb_each(p_details) LOOP
            IF v_key IN ('session_id', 'harness_id', 'harness_host') THEN
                CONTINUE;
            END IF;
            IF v_key NOT IN ('op', 'device_id', 'key_fp', 'reason', 'code', 'len', 'sha256', 'channel',
                    'devices', 'sessions') THEN
                RAISE EXCEPTION 'relay_record_audit: unexpected detail %', v_key USING ERRCODE = '22023';
            END IF;
            IF jsonb_typeof(v_val) NOT IN ('string', 'number', 'boolean', 'null')
               OR (jsonb_typeof(v_val) = 'string' AND length(v_val #>> '{}') > 200) THEN
                RAISE EXCEPTION 'relay_record_audit: detail % must be a short scalar', v_key USING ERRCODE = '22023';
            END IF;
            v_details := v_details || jsonb_build_object(v_key, v_val);
        END LOOP;
    END IF;
    IF p_session_id IS NOT NULL THEN
        v_details := v_details || jsonb_build_object('session_id', p_session_id, 'harness_id', v_harness,
            'harness_host', v_host);
        v_summary := format('%s: session %s on %s', p_action, p_session_id, v_host);
    ELSE
        v_summary := p_action;
    END IF;

    INSERT INTO audit_log (actor_user_id, actor_email, via_token, org_id, project_id, action, target_type,
                           target_id, summary, details)
    VALUES (p_actor_user_id, COALESCE(v_email, ''), FALSE, v_org, v_project, p_action,
            CASE WHEN p_session_id IS NULL THEN '' ELSE 'remote_session' END, COALESCE(p_session_id, ''),
            v_summary, v_details)
    RETURNING id, at INTO v_id, v_at;
    INSERT INTO relay_audit_outbox (audit_id) VALUES (v_id);
    RETURN QUERY SELECT v_id, v_at;
END
$fn$;

-- Functions are executable by PUBLIC by default; only the relay's role
-- (GrantRelayRole) and the owner may call this one.
REVOKE ALL ON FUNCTION relay_record_audit(INT, TEXT, TEXT, JSONB) FROM PUBLIC;
