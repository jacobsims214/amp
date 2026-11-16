-- Team submit grants: who may submit a request to a team's inbox without being
-- able to reach any of that team's projects (AMP #3768; approved contract
-- artifact 36 v2 §2 and decision D14, build slice B2's storage half).
--
-- The problem this table exists for. Today every project-scoped capability
-- starts at `Level >= Member`, and a person reaches a project only through
-- team membership (`AccessibleProjectIDs`), or as an org owner/admin. So the
-- only way to let an outsider submit would be to add them to a linked team —
-- which hands them `TaskRead` on every ticket, comment and history row in the
-- project. The contract forbids exactly that ("submitting is a capability
-- outside the project level ladder"). A submit grant is therefore its own
-- relation, keyed to a team, that grants *submitting* and nothing else.
--
-- The rule this table must never break, and the reason its write path is one
-- INSERT: **granting submit access creates no membership row.** It does not
-- touch org_members or team_members. A customer with a grant stays a customer
-- with zero project visibility; the authorization layer checks this relation
-- directly (slice B3's `Checker.TeamSubmit`), re-reading it on every call
-- rather than consulting any project-keyed allow-set.
--
-- One row per (team, user), and revocation is a *state*, not a delete:
--
--   granted_by/granted_at   who gave the access, and when it took effect
--   revoked_by/revoked_at   who took it away, and when it stopped
--
-- Two consequences follow, and both are deliberate.
--
-- First, duplicate grants are idempotent by construction. The primary key
-- (team_id, user_id) makes a second row for the same pair unstorable, and the
-- repository's upsert keeps the original grantor and time when the grant is
-- already active, so a replayed grant is observably a no-op instead of
-- resetting attribution. Re-granting a *revoked* grant revives the row with
-- the new grantor and time: the access a person holds is the one currently in
-- force, and the history of who granted and revoked what lives in the audit
-- log (slice B2's service half, #3769), which is where every other append-only
-- record of an act in AMP lives.
--
-- Second, revocation is effective on the next read with no cache to
-- invalidate. `RevokeTeamSubmit` is a single `UPDATE ... WHERE revoked_at IS
-- NULL`, so it is atomic (one of two concurrent revocations changes the row,
-- the other is a no-op) and every read below filters `revoked_at IS NULL`. The
-- grant row is never removed on revocation, so there is no delete-then-insert
-- window in which a re-grant and a stale allow-set could disagree.
--
-- Tenancy is a constraint, not a convention. A grant names its org as well as
-- its team, and `FOREIGN KEY (team_id, org_id) REFERENCES teams (id, org_id)`
-- (backed by 053's `uq_teams_id_org`) refuses any row whose team is not in
-- that org. A write path — or a hand fix, or a future adapter — that passes a
-- team from another tenant is refused by the database rather than stored as a
-- cross-tenant grant. Deleting the team (or its org) takes its grants with it:
-- a grant has no meaning without the inbox it opens.
--
-- The recipient is a `users` row, typed rather than an email. An email-keyed
-- grant would have to be re-pointed when the person signs in, and a renamed
-- address would silently carry or drop access; a `users` row is the identity
-- the authorization check actually has in hand. `ON DELETE CASCADE` is the
-- opposite of the request record's `ON DELETE SET NULL` on purpose: a request
-- must survive its requester leaving (there is still a person to answer), but
-- a grant is access *to that user* and means nothing once the user is gone.
-- Both attribution columns are `ON DELETE SET NULL` so deleting a manager
-- never deletes the grant they made, and the CHECK below is one-way
-- (`revoked_by IS NULL OR revoked_at IS NOT NULL`) so it still holds once the
-- actor goes NULL — the trap the request schema documents.
--
-- This migration adds one table and one index. It adds nothing to teams,
-- users, team_requests or any other existing table, so 053's pinned
-- nine-index count on `team_requests` is unaffected. Additive and idempotent:
-- re-running the file leaves the same table, constraints and indexes.

CREATE TABLE IF NOT EXISTS team_submit_grants (
    team_id    INT NOT NULL,
    -- Stated by the writer and checked against the team, never inferred.
    org_id     INT NOT NULL,
    -- The recipient: the identity the submit check has in hand.
    user_id    INT NOT NULL
        CONSTRAINT team_submit_grants_user
        REFERENCES users(id) ON DELETE CASCADE,

    -- Who granted it, and when it took effect. NOT NULL time, nullable actor
    -- (a deleted manager never deletes the access they gave).
    granted_by INT REFERENCES users(id) ON DELETE SET NULL,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Who took it away, and when it stopped. A revoked row stays: it is the
    -- record that this person once held the grant, and it is what a re-grant
    -- revives.
    revoked_by INT REFERENCES users(id) ON DELETE SET NULL,
    revoked_at TIMESTAMPTZ,

    -- One grant per person per team. Duplicate grants are unstorable rather
    -- than deduplicated by convention.
    PRIMARY KEY (team_id, user_id),

    -- The team must belong to the grant's org. This is the tenant guard.
    CONSTRAINT team_submit_grants_team_in_org
        FOREIGN KEY (team_id, org_id) REFERENCES teams (id, org_id) ON DELETE CASCADE,

    -- Recording who revoked requires recording when. Written as a one-way
    -- implication so it still holds when revoked_by is nulled by a user
    -- deletion; the timestamp, not the actor, is what the access rule reads.
    CONSTRAINT team_submit_grants_revocation_actor
        CHECK (revoked_by IS NULL OR revoked_at IS NOT NULL)
);

-- The primary key covers "every grant on this team" and "this person's grant
-- on this team". This partial index covers the other read the capability needs:
-- "every team this person may submit to", and only while the grant is in
-- force — a revoked grant is not access and is never part of that answer.
CREATE INDEX IF NOT EXISTS idx_team_submit_grants_user
    ON team_submit_grants (user_id) WHERE revoked_at IS NULL;
