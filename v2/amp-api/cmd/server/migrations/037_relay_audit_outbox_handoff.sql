-- amp-relay's audit entries are delivered through audit_outbox (AMP #2433).
--
-- relay_audit_outbox (030, #2399) queued relay_record_audit's rows for a
-- separate webhook loop in amp-api. audit_outbox (033, #2429) queues every
-- audit_log row from a trigger, relay rows included, and since 036
-- relay_record_audit stops queueing in relay_audit_outbox once audit_outbox
-- exists. amp-api no longer drains relay_audit_outbox, so this hands over
-- whatever was still queued there (rows written before the upgrade, or by a
-- relay call that raced the upgrade) and empties it. Webhook receivers
-- de-duplicate on the envelope id (evt_audit_<id>), so a row an old replica
-- delivered while this ran is harmless.
--
-- The table stays (a rolling deploy's older amp-api still reads it). Idempotent.
INSERT INTO audit_outbox (audit_id)
SELECT audit_id FROM relay_audit_outbox
ON CONFLICT (audit_id) DO NOTHING;

DELETE FROM relay_audit_outbox;
