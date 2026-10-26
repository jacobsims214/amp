-- Audit log v2 coverage (KB decisions/audit-log-v2.md, ticket #2428).
-- Additive only: two partial indexes for the lookups the new events make on
-- the request path, so neither scans the table.
--   * auth.login is written once per Dex session: before writing one, a
--     replica checks whether any replica already did.
--   * token.use.new_ip is written the first time an API token is used from
--     an address not seen for it in 30 days.
-- Numbered 032: #2427 is 031, #2429 (integrity) is 033.
CREATE INDEX IF NOT EXISTS idx_audit_login_session ON audit_log(session_id) WHERE action = 'auth.login';
CREATE INDEX IF NOT EXISTS idx_audit_token_ip ON audit_log(token_id, source_ip, at) WHERE token_id IS NOT NULL;
