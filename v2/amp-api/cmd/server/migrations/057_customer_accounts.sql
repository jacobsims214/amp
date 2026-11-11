-- Customer (requester) accounts: the third users.kind value (AMP #3766,
-- approved contract artifact 36 v2, build slice B1).
--
-- A customer is an authenticated requester identity for team intake. It is
-- deliberately *not* an org or project member by implication: the intake
-- surface grants submitting through its own permission (a later slice), so a
-- customer's account kind alone must reach no project.
--
-- The reason this CHECK matters is the one thing kind must exclude: an org
-- owner/admin reaches every project in the org (`levelFromAccess`), which is
-- exactly what a requester must never get. Application paths refuse an
-- owner/admin grant to a customer (service + repository guards, see
-- domain.CanHoldOrgRole and repository.guardCustomerOrgRole); this CHECK keeps
-- the vocabulary closed so no other spelling of an account kind can appear.
--
-- Migration 009 added users.kind with a default and no constraint, so this is
-- the table's first kind CHECK rather than a widening of an existing one. It
-- accepts exactly the rows that already exist (human, agent) plus customer, so
-- it validates as-is on existing data.
--
-- Additive and idempotent: re-running the file leaves the same single
-- constraint. No existing row is converted and no human becomes a customer by
-- this migration.

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_kind_check;
ALTER TABLE users ADD CONSTRAINT users_kind_check
    CHECK (kind IN ('human', 'agent', 'customer'));
