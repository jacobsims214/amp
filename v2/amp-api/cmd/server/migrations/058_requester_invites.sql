-- Requester invitations: an invite that provisions a *customer* identity
-- instead of org/team membership (AMP #3767, approved contract artifact 36 v2
-- §5, build slice B1's invite half).
--
-- A requester invite is a different intent from a membership invite, and the
-- difference has to survive until the person first signs in: at that moment
-- the account is provisioned (repository.users) and the invite is applied
-- (repository.access). Until now both paths only knew one intent, so a
-- customer targeted by an invite would have become a human member with team
-- membership, which is exactly what the contract forbids.
--
-- kind = 'member' is the existing behaviour and stays the default, so every
-- row that already exists keeps its meaning and nothing changes for it.
-- kind = 'requester' means: the new identity is provisioned as a customer
-- (`users.kind = 'customer'`), and accepting the invite records acceptance
-- only — no org_members or team_members row. Team submit access is granted
-- separately by the submit-grant slice (#3768), never by acceptance.
--
-- The pending unique index (idx_invites_pending, migration 004) is untouched:
-- re-inviting the same email to the same org/team still replaces the pending
-- row, and this migration makes the replacement carry the new intent too, so
-- a replay is last-write-wins rather than additive. Revoking deletes the
-- pending row, so a revoked invite can never be accepted.
--
-- Additive and idempotent: re-running the file leaves the same column and the
-- same single constraint.

ALTER TABLE invites ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'member';

ALTER TABLE invites DROP CONSTRAINT IF EXISTS invites_kind_check;
ALTER TABLE invites ADD CONSTRAINT invites_kind_check
    CHECK (kind IN ('member', 'requester'));
