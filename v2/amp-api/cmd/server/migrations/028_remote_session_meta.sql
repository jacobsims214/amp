-- Remote control: the session list's metadata (AMP #2389, epic 165).
-- Spec: v2/docs/remote-control/protocol.md sections 6.3 and 8.3 (GET /api/sessions).
--
-- Additive and idempotent: three nullable columns on remote_sessions and one
-- index. Nothing existing changes, an older amp-relay ignores the columns, and
-- running it again changes nothing.
--
-- These are the only status members the relay stores, copied from the
-- harness's signed status (hello.body.status or a status event): the model
-- name and the current ticket's id and name. They are metadata like the host
-- name. No message content (prompts, replies, tool output) is ever stored (D3).
ALTER TABLE remote_sessions ADD COLUMN IF NOT EXISTS model TEXT;
ALTER TABLE remote_sessions ADD COLUMN IF NOT EXISTS current_ticket_id BIGINT;
ALTER TABLE remote_sessions ADD COLUMN IF NOT EXISTS current_ticket_name TEXT;

-- GET /api/sessions lists the owner's sessions that ended in the last 7 days.
CREATE INDEX IF NOT EXISTS remote_sessions_owner_ended ON remote_sessions (owner_user_id, ended_at DESC)
    WHERE state = 'ended';
