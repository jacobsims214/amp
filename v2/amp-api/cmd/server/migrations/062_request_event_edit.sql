-- Team requests: the `edit` history verb (AMP #3896, epic 202, story 535; the
-- follow-up #3873 documented, converging on the approved contract artifact 36
-- v2 §1 — "current rev updates atomically with edit event").
--
-- The problem this closes. 054 makes `request_events.action` a closed
-- vocabulary (submit, transition, claim, release, assign, link, unlink) with a
-- named CHECK, mirrored by domain.RequestEventActions. None of those verbs means
-- "the request's content changed", so #3873 (the versioned content edit,
-- `Repo.UpdateRequest`) recorded its edit as a `request_comments` row of kind
-- `system` — the shape 054 defines for "what a transition or a connector writes
-- on the request's behalf". The provenance, append-only and atomicity invariants
-- held, but the record shape did not: an edit is an act, and 054's own split
-- puts acts in the team's history rail (`request_events`), not in the requester's
-- thread. #3873 could not widen the vocabulary because the story designated one
-- migration writer (061) and no other slot was free. 061 has landed, so this is
-- 062.
--
-- The change is one word in one closed set: `edit`. It is additive and it alters
-- nothing else. `request_events_transition_states` already says exactly what an
-- `edit` row carries — action <> 'transition' means from_state = '' and
-- to_state = '' — so no second constraint is needed, and an `edit` row can never
-- smuggle in a state change that did not happen. The row's `detail` names the
-- changed fields and the revision transition (field names and numbers only, never
-- the brief text), because a request's history is requester-visible.
--
-- Idempotent in the house style: drop the named constraint if it exists, then
-- re-add it. Re-running this file by hand is a no-op, which matters because the
-- unversioned boot path re-runs migration SQL. Widening a CHECK re-validates the
-- table's existing rows, and it cannot fail: every action already stored is in
-- the wider set.

ALTER TABLE request_events DROP CONSTRAINT IF EXISTS request_events_action;
ALTER TABLE request_events ADD CONSTRAINT request_events_action
    CHECK (action IN ('submit', 'transition', 'claim', 'release', 'assign', 'link', 'unlink', 'edit'));
