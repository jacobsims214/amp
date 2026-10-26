-- Relay audit rows carry request context (AMP #2433, KB
-- decisions/audit-log-v2.md section 6).
--
-- relay_record_audit (030, #2399) gains a five-argument version that also
-- takes the request context amp-relay resolved on its public listener
-- (internal/reqctx): the client IP, the user agent, client = relay, the
-- request id, and how the caller signed in. Every #2399 check stays: only
-- the relay's own remote.* actions, about a session the actor owns, with
-- short scalar details from a fixed list; the function still fills email,
-- org, project, target and summary itself. The context is validated as
-- strictly:
--   ip          a single host address (IPv4 or IPv6, no prefix)
--   user_agent  at most 512 characters
--   client      'relay' (the only value; the column is always 'relay')
--   request_id  1-128 of [A-Za-z0-9-]
--   auth_method dex_session or oauth_mcp (the relay refuses API tokens),
--               and only with an actor
--   session_id  1-64 lower-case hex (the Dex session id), only with an actor
-- and nothing else. The function sets actor_type (from users.kind, or
-- system when there is no actor), schema_version 2, and outcome denied for
-- remote.refused. A relay can still lie about its own requests' context;
-- the validation bounds what it can write.
--
-- The four-argument version stays for a rolling deploy (an older relay
-- calls it) and is now the five-argument one with no context.
--
-- relay_audit_outbox: amp-api's webhook dispatcher drains it. When the
-- durable audit_outbox exists (#2429, migration 033), its trigger already
-- queues every audit_log row, relay rows included, so the function stops
-- queueing here; that check is made per call, so the migrations may arrive
-- in either order.
--
-- Additive and idempotent.

CREATE OR REPLACE FUNCTION relay_record_audit(
    p_actor_user_id INT,
    p_session_id    TEXT,
    p_action        TEXT,
    p_details       JSONB,
    p_ctx           JSONB
) RETURNS TABLE (audit_id BIGINT, audit_at TIMESTAMPTZ)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
DECLARE
    v_email      TEXT := '';
    v_kind       TEXT;
    v_owner      INT;
    v_project    INT;
    v_org        INT;
    v_host       TEXT := '';
    v_harness    TEXT;
    v_details    JSONB := '{}'::jsonb;
    v_key        TEXT;
    v_val        JSONB;
    v_summary    TEXT;
    v_id         BIGINT;
    v_at         TIMESTAMPTZ;
    v_ip         INET;
    v_ua         TEXT;
    v_request_id TEXT;
    v_auth       TEXT := 'none';
    v_dex_sid    TEXT;
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
        SELECT u.email, u.kind INTO v_email, v_kind FROM users u WHERE u.id = p_actor_user_id;
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

    -- Request context: known members only, each validated.
    IF p_ctx IS NOT NULL AND jsonb_typeof(p_ctx) <> 'null' THEN
        IF jsonb_typeof(p_ctx) <> 'object' THEN
            RAISE EXCEPTION 'relay_record_audit: context must be an object' USING ERRCODE = '22023';
        END IF;
        FOR v_key, v_val IN SELECT * FROM jsonb_each(p_ctx) LOOP
            IF v_key NOT IN ('ip', 'user_agent', 'client', 'request_id', 'auth_method', 'session_id') THEN
                RAISE EXCEPTION 'relay_record_audit: unexpected context %', v_key USING ERRCODE = '22023';
            END IF;
            IF jsonb_typeof(v_val) <> 'string' THEN
                RAISE EXCEPTION 'relay_record_audit: context % must be a string', v_key USING ERRCODE = '22023';
            END IF;
        END LOOP;
        IF p_ctx ? 'ip' THEN
            BEGIN
                v_ip := (p_ctx ->> 'ip')::inet;
            EXCEPTION WHEN invalid_text_representation THEN
                RAISE EXCEPTION 'relay_record_audit: context ip is not an address' USING ERRCODE = '22023';
            END;
            IF masklen(v_ip) <> (CASE family(v_ip) WHEN 4 THEN 32 ELSE 128 END) THEN
                RAISE EXCEPTION 'relay_record_audit: context ip must be one address' USING ERRCODE = '22023';
            END IF;
        END IF;
        IF p_ctx ? 'user_agent' THEN
            v_ua := p_ctx ->> 'user_agent';
            IF length(v_ua) > 512 THEN
                RAISE EXCEPTION 'relay_record_audit: context user_agent is too long' USING ERRCODE = '22023';
            END IF;
        END IF;
        IF p_ctx ? 'client' AND p_ctx ->> 'client' <> 'relay' THEN
            RAISE EXCEPTION 'relay_record_audit: context client must be relay' USING ERRCODE = '22023';
        END IF;
        IF p_ctx ? 'request_id' THEN
            v_request_id := p_ctx ->> 'request_id';
            IF v_request_id !~ '^[A-Za-z0-9-]{1,128}$' THEN
                RAISE EXCEPTION 'relay_record_audit: context request_id is malformed' USING ERRCODE = '22023';
            END IF;
        END IF;
        IF p_ctx ? 'auth_method' THEN
            IF p_actor_user_id IS NULL OR p_ctx ->> 'auth_method' NOT IN ('dex_session', 'oauth_mcp') THEN
                RAISE EXCEPTION 'relay_record_audit: context auth_method is not one the relay accepts'
                    USING ERRCODE = '22023';
            END IF;
            v_auth := p_ctx ->> 'auth_method';
        END IF;
        IF p_ctx ? 'session_id' THEN
            v_dex_sid := p_ctx ->> 'session_id';
            IF p_actor_user_id IS NULL OR v_dex_sid !~ '^[0-9a-f]{1,64}$' THEN
                RAISE EXCEPTION 'relay_record_audit: context session_id is malformed' USING ERRCODE = '22023';
            END IF;
        END IF;
    END IF;

    IF p_session_id IS NOT NULL THEN
        v_details := v_details || jsonb_build_object('session_id', p_session_id, 'harness_id', v_harness,
            'harness_host', v_host);
        v_summary := format('%s: session %s on %s', p_action, p_session_id, v_host);
    ELSE
        v_summary := p_action;
    END IF;

    INSERT INTO audit_log (actor_user_id, actor_email, via_token, org_id, project_id, action, target_type,
                           target_id, summary, details,
                           schema_version, actor_type, auth_method, session_id, outcome, source_ip, user_agent,
                           client, request_id)
    VALUES (p_actor_user_id, COALESCE(v_email, ''), FALSE, v_org, v_project, p_action,
            CASE WHEN p_session_id IS NULL THEN '' ELSE 'remote_session' END, COALESCE(p_session_id, ''),
            v_summary, v_details,
            2,
            CASE WHEN p_actor_user_id IS NULL THEN 'system' WHEN v_kind = 'agent' THEN 'agent' ELSE 'human' END,
            v_auth, v_dex_sid,
            CASE WHEN p_action = 'remote.refused' THEN 'denied' ELSE 'success' END,
            v_ip, v_ua, 'relay', v_request_id)
    RETURNING id, at INTO v_id, v_at;
    -- With the durable audit_outbox (033) its trigger has queued the row
    -- already; queueing it here too would deliver it twice.
    IF to_regclass('public.audit_outbox') IS NULL THEN
        INSERT INTO relay_audit_outbox (audit_id) VALUES (v_id);
    END IF;
    RETURN QUERY SELECT v_id, v_at;
END
$fn$;

-- The four-argument version (030), for relays that don't send context yet.
CREATE OR REPLACE FUNCTION relay_record_audit(
    p_actor_user_id INT,
    p_session_id    TEXT,
    p_action        TEXT,
    p_details       JSONB
) RETURNS TABLE (audit_id BIGINT, audit_at TIMESTAMPTZ)
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
    SELECT * FROM relay_record_audit(p_actor_user_id, p_session_id, p_action, p_details, NULL::jsonb)
$fn$;

-- Functions are executable by PUBLIC by default; only the relay's role
-- (GrantRelayRole) and the owner may call these.
REVOKE ALL ON FUNCTION relay_record_audit(INT, TEXT, TEXT, JSONB) FROM PUBLIC;
REVOKE ALL ON FUNCTION relay_record_audit(INT, TEXT, TEXT, JSONB, JSONB) FROM PUBLIC;
