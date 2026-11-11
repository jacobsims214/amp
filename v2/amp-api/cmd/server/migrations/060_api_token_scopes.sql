-- API token scopes: a credential that may only submit to one team's inbox
-- (AMP #3772; approved contract artifact 36 v2 §5 and #3660, build slice B5 of
-- discoveries/team-inbox-approved-build-slices.md).
--
-- The problem. A token is otherwise a full stand-in for the account behind it:
-- the bearer acts with that person's whole authority, everywhere, until the
-- token is revoked. A connector or service client whose only job is to send
-- requests to one team's inbox therefore holds the account's entire reach —
-- every project it can see, every ticket it could edit, every grant it could
-- administer. A leaked connector credential should cost one inbox's worth of
-- junk, not the project.
--
-- The scope is stored on the credential itself rather than in a new table, so
-- it is resolved with the token lookup that already happens on every request:
-- one row, one indexed read, no second store to disagree with api_tokens about
-- what a token may do, and no new auth transport (the bearer header is
-- unchanged). It is also what makes the scope unescalatable: api_tokens has no
-- UPDATE that touches these columns — the mint inserts them and revocation
-- sets `revoked_at`; nothing else writes the row. A scoped token cannot widen
-- itself because there is no path, in any handler, that rewrites `scope`.
--
-- The two scopes are closed (domain.TokenScopeFull, domain.TokenScopeSubmit;
-- the same list domain.TokenScopes names), and the CHECKs enforce four
-- properties that must not be expressible as rows:
--
--   1. `scope` is one of the two values. A typo, or a future value deployed
--      before its code, is unstorable rather than silently meaning "full" —
--      the safe default for a credential is the narrower one.
--   2. A submit scope always names its one team. `scope = 'submit'` requires
--      `scope_team_id IS NOT NULL`; a submit token with no team would be a
--      credential whose only permitted operation has no target.
--   3. A full scope never carries a team. `scope = 'full'` requires
--      `scope_team_id IS NULL`, so a "full" token can never quietly become
--      team-bound by a later code path reading a stale column, and there is
--      exactly one representation of each scope.
--   4. The team must exist, and deleting the team deletes the credential
--      (`ON DELETE CASCADE`). A submit-only token for a team that no longer
--      exists can submit nowhere: it is not a token that silently becomes
--      useless-but-live, and it is not one whose team id could be reused by a
--      later team (serial ids are not reused, but a cascade makes the
--      invariant hold without reasoning about that at all). This mirrors
--      team_submit_grants' cascade: both mean "access to that team", and
--      neither outlives its team.
--
-- Existing rows are exactly right under the default: every token minted before
-- this migration has `scope = 'full'` and no team, which is the behaviour it
-- already had. This migration rewrites no row, adds two nullable-by-design
-- columns and one CHECK, and is additive and idempotent: re-running the file
-- leaves the same columns, constraint and index.
--
-- The team index exists for the one question the schema can be asked about
-- these rows that isn't already served: "which live credentials point at this
-- team" (the token list a team manager would read). The per-request
-- enforcement never asks it — it reads the token by hash, which the existing
-- unique index on token_hash serves.

ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT 'full';
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS scope_team_id INT
    REFERENCES teams(id) ON DELETE CASCADE;

-- One CHECK holding the closed vocabulary and the two implications. Written as
-- a single constraint so no interleaving of DDL can leave the table accepting
-- a row the others would reject.
ALTER TABLE api_tokens DROP CONSTRAINT IF EXISTS api_tokens_scope_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_scope_check
    CHECK (
        scope IN ('full', 'submit')
        AND (scope <> 'submit' OR scope_team_id IS NOT NULL)
        AND (scope <> 'full' OR scope_team_id IS NULL)
    );

CREATE INDEX IF NOT EXISTS idx_api_tokens_scope_team
    ON api_tokens (scope_team_id) WHERE scope_team_id IS NOT NULL;
