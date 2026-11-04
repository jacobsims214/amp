-- Project-scoped repository-facts cache (epic 183, story 453, AMP #3152).
-- The merge-alert policy's missing input: decisions/repo-default-branch.md §2.
--
-- A merge reaches a person only when its PR targets the repository's DETECTED
-- default branch, or a repository rule actually requires their approval.
-- Deciding that needs things AMP does not hold today: a repository's default
-- branch, the rules evaluated against a PR's target branch, and a few
-- PR-level facts (actual target, requested reviewers, review decision,
-- mergeable state). Reading them per card on every My work read is a page of
-- GitHub calls, so they are cached here. Three properties are enforced by the
-- schema itself, not only by the code that writes it:
--
-- 1. NOTHING IS GLOBAL. Every fact row belongs to exactly one project, and the
--    credential recorded on it must be a credential of that same project (and
--    host). The same owner/repo in two projects is two independent rows, so
--    facts obtained with one project's token — and the private-repository
--    visibility that token carries — can never be read through another
--    project. No fact row is keyed by slug alone, and no uniqueness is
--    declared across projects. There is deliberately no global
--    repo_slug-keyed table: token-derived visibility is never shared.
--
-- 2. NO SECRET IS STORED HERE. None of these tables has a secret-bearing
--    column: the token itself lives only in the sealed credential storage
--    added with the project credential service (AMP #3154), which extends
--    project_github_credentials rather than introducing a second table. Facts
--    reference a credential by id. The project metadata keys this cache works
--    with (github_repo, github_default_branch) name a repository, never a
--    credential. Failures are stored as a class from a fixed list — never a
--    message, body or header — so a raw error string carrying a token cannot
--    be written into a row by accident. Widening the list needs a migration,
--    on purpose (as with kb_documents.change).
--
-- 3. NOT KNOWING IS REPRESENTABLE. completeness starts at 'unknown' and
--    fetched_at stays NULL until a read actually succeeds, so "never checked",
--    "checked but partial" and "checked and clean" are distinct states. A
--    failed refresh keeps the previous verified fetched_at, so stale facts
--    stay visibly stale instead of looking freshly checked. That is what stops
--    an unreadable private repo, a 404, a 403 or a rate limit from ever being
--    read as "no rule applies".
--
-- Additive only: new tables, no column and no backfill on an existing table.
-- Idempotent (the runner applies it once; the IF NOT EXISTS guards keep a
-- manual re-run harmless). The tables are small — one row per repository, per
-- evaluated branch and per open PR, per project — so their primary and unique
-- keys are the read path.

-- Which credential context a project's repository facts were obtained with.
--
-- Reference metadata only: host, label, lifecycle timestamps and who attached
-- it. No token, no hash of a token, no scope list, nothing to read back. The
-- sealed secret and its attachment/rotation audit are the credential
-- service's (AMP #3154).
--
-- A row here means an ACTIVE credential context. Revoking or replacing a
-- credential deletes its row rather than flagging it: there is deliberately no
-- revoked_at, because a flagged row that stays would leave the facts it
-- produced readable after the token behind them was withdrawn. Deleting is
-- what cascades (see github_repo_facts), so "this credential is gone" and
-- "nothing it read is visible any more" are the same event and cannot drift
-- apart. Who attached, rotated or deleted it is the audit log's record, not a
-- column here.
CREATE TABLE IF NOT EXISTS project_github_credentials (
    id         SERIAL PRIMARY KEY,
    project_id INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- The API host this credential is valid for. Another host (an enterprise
    -- install) stays explicitly unsupported until there is a host/credential
    -- decision, but it is recorded so facts from two hosts never collide.
    host       TEXT NOT NULL DEFAULT 'github.com',
    label      TEXT NOT NULL DEFAULT '',
    created_by INT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- When the secret was last replaced. NULL: never rotated since attached.
    -- A rotation is an in-place update, so it does not disturb the facts the
    -- same project already holds: they are still this project's own view.
    rotated_at TIMESTAMPTZ,
    CONSTRAINT project_github_credentials_host_check
        CHECK (host <> '' AND length(host) <= 253),
    -- One credential context per project and host: "rotate" replaces it in
    -- place, and deleting it drops the facts it produced (below).
    CONSTRAINT project_github_credentials_project_host_key UNIQUE (project_id, host),
    -- The target of the facts' isolation foreign key: it is what proves a
    -- fact row's credential belongs to its own project and host.
    CONSTRAINT project_github_credentials_identity_key UNIQUE (id, project_id, host)
);

-- The repository-level facts: its identity and its default branch.
CREATE TABLE IF NOT EXISTS github_repo_facts (
    id            SERIAL PRIMARY KEY,
    project_id    INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    host          TEXT NOT NULL DEFAULT 'github.com',
    -- owner/repo — never a URL and never a path. The slug is the identity, so
    -- a URL-shaped or malformed value is refused instead of quietly becoming
    -- a second row for the same repository. Stored as the reader reports it
    -- (githubfacts.ParsePullURL / validSlug, AMP #3151) rather than
    -- re-cased here, so the value AMP holds is the value GitHub gave it; the
    -- uniqueness below is case-insensitive to match GitHub, which treats
    -- 'Owner/Repo' and 'owner/repo' as one repository.
    repo_slug     TEXT NOT NULL,
    -- The credential these facts were read with. NULL means read with none (a
    -- public repository, or no token attached yet): a weaker claim the
    -- classifier must treat as such, never as "private and verified".
    credential_id INT,
    default_branch        TEXT NOT NULL DEFAULT '',
    -- Where default_branch came from. 'manual' is the project owner's override
    -- and wins; 'unknown' means not detected, and is never guessed at.
    default_branch_source TEXT NOT NULL DEFAULT 'unknown',
    -- Whether the last successful read saw everything it asked for:
    -- 'complete' (default branch and rules read), 'partial' (some surface,
    -- e.g. classic branch protection, was not readable with this token) or
    -- 'unknown' (never read successfully).
    completeness  TEXT NOT NULL DEFAULT 'unknown',
    -- When these facts were last VERIFIED. NULL: never. A reader compares it
    -- with its TTL; a failed refresh leaves it alone, so staleness shows.
    fetched_at    TIMESTAMPTZ,
    -- Why the last attempt failed, as a class only. '' means it didn't.
    last_error_class TEXT NOT NULL DEFAULT '',
    last_error_at    TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT github_repo_facts_slug_check
        CHECK (repo_slug ~ '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' AND length(repo_slug) <= 200),
    CONSTRAINT github_repo_facts_source_check
        CHECK (default_branch_source IN ('unknown', 'rest', 'graphql', 'origin_head', 'manual')),
    -- A source other than 'unknown' is a claim, so it must name a branch.
    CONSTRAINT github_repo_facts_detected_has_branch_check
        CHECK (default_branch_source = 'unknown' OR default_branch <> ''),
    CONSTRAINT github_repo_facts_completeness_check
        CHECK (completeness IN ('unknown', 'partial', 'complete')),
    -- 'unknown' is the never-checked state, so it cannot carry a verification
    -- time; 'partial' and 'complete' both did read something worth dating.
    CONSTRAINT github_repo_facts_completeness_dated_check
        CHECK (completeness = 'unknown' OR fetched_at IS NOT NULL),
    CONSTRAINT github_repo_facts_failure_class_check
        CHECK (last_error_class IN ('', 'not_found', 'forbidden',
                                    'rate_limited', 'timeout', 'canceled', 'network',
                                    'server', 'moved', 'invalid', 'no_credential',
                                    'no_target', 'unsupported_host')),
    -- Row-level isolation (one row per project, host and repository) is the
    -- case-insensitive unique index below; the credential cross-reference is
    -- the foreign key here.
    -- Isolation, at the cross-reference level: a credential can only be
    -- recorded against facts of its own project and host. With credential_id
    -- NULL the check does not apply (MATCH SIMPLE), which is the anonymous
    -- case above. Deleting the credential drops the facts it produced, so no
    -- token-derived visibility outlives its token.
    CONSTRAINT github_repo_facts_credential_fk
        FOREIGN KEY (credential_id, project_id, host)
        REFERENCES project_github_credentials(id, project_id, host) ON DELETE CASCADE
);

-- Isolation, at the row level: one fact row per project, host and repository.
-- Case-insensitive (lower(repo_slug)) because GitHub treats an owner and a
-- repository name case-insensitively, so 'Owner/Repo' and 'owner/repo' are one
-- repository and must be one row — one credential context, one answer — rather
-- than two that could disagree. A unique *index* rather than a UNIQUE
-- constraint so the authoring is explicit and the rule is stated once, here.
CREATE UNIQUE INDEX IF NOT EXISTS uq_github_repo_facts_identity
    ON github_repo_facts(project_id, host, lower(repo_slug));

CREATE INDEX IF NOT EXISTS idx_github_repo_facts_credential
    ON github_repo_facts(credential_id) WHERE credential_id IS NOT NULL;

-- The rules evaluated against one branch of one repository: what the
-- classifier reads to decide whether a rule (rather than the default branch)
-- makes a merge actionable. Branch names are free-form (slashes, dots), so
-- this is keyed by the repository row: a branch fact inherits its project and
-- credential context from it and cannot sit under another project's repo.
CREATE TABLE IF NOT EXISTS github_branch_facts (
    repo_facts_id INT NOT NULL REFERENCES github_repo_facts(id) ON DELETE CASCADE,
    branch        TEXT NOT NULL,
    -- The evaluated rules, reduced to what the classifier reads: type (only
    -- 'pull_request' can demand a review), required_approving_review_count,
    -- require_code_owner_review, required_reviewers, require_last_push_approval,
    -- require_extra_approval_for_unattributed_changes,
    -- require_review_thread_resolution, ruleset_id, ruleset_source,
    -- ruleset_source_type. An array, never a whole response envelope.
    rules         JSONB NOT NULL DEFAULT '[]',
    -- 'partial' is the honest state for "rules read, classic branch protection
    -- not readable with this token": a positive signal for a rule, never proof
    -- that no rule applies.
    completeness  TEXT NOT NULL DEFAULT 'unknown',
    fetched_at    TIMESTAMPTZ,
    last_error_class TEXT NOT NULL DEFAULT '',
    last_error_at    TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (repo_facts_id, branch),
    CONSTRAINT github_branch_facts_branch_check CHECK (branch <> '' AND length(branch) <= 255),
    CONSTRAINT github_branch_facts_rules_check CHECK (jsonb_typeof(rules) = 'array'),
    CONSTRAINT github_branch_facts_completeness_check
        CHECK (completeness IN ('unknown', 'partial', 'complete')),
    CONSTRAINT github_branch_facts_completeness_dated_check
        CHECK (completeness = 'unknown' OR fetched_at IS NOT NULL),
    CONSTRAINT github_branch_facts_failure_class_check
        CHECK (last_error_class IN ('', 'not_found', 'forbidden',
                                    'rate_limited', 'timeout', 'canceled', 'network',
                                    'server', 'moved', 'invalid', 'no_credential',
                                    'no_target', 'unsupported_host'))
);

-- The PR-level facts. They change on every reviewer action, so they carry
-- their own timestamps and a much shorter TTL than repository or branch
-- facts. The PR number is scoped to its repository row, which carries the
-- project.
CREATE TABLE IF NOT EXISTS github_pull_facts (
    repo_facts_id INT NOT NULL REFERENCES github_repo_facts(id) ON DELETE CASCADE,
    number        INT NOT NULL,
    -- The PR's actual target, as GitHub reports it. A run's base_branch is
    -- what the agent said; this is what confirms or corrects it.
    base_branch   TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'unknown',
    -- GitHub's reviewDecision: whether an approval is still outstanding.
    review_decision TEXT NOT NULL DEFAULT 'unknown',
    -- Who GitHub has asked. An over-approximation on its own — code owners are
    -- requested for review even with no rule protecting the branch — so it
    -- only counts alongside a rules read.
    requested_reviewers TEXT[] NOT NULL DEFAULT '{}',
    requested_teams     TEXT[] NOT NULL DEFAULT '{}',
    -- GitHub's mergeable_state; 'blocked' is the fallback positive signal when
    -- the rules could not be read. Anything unrecognised stays 'unknown'.
    mergeable_state TEXT NOT NULL DEFAULT 'unknown',
    completeness  TEXT NOT NULL DEFAULT 'unknown',
    fetched_at    TIMESTAMPTZ,
    last_error_class TEXT NOT NULL DEFAULT '',
    last_error_at    TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (repo_facts_id, number),
    CONSTRAINT github_pull_facts_number_check CHECK (number > 0),
    CONSTRAINT github_pull_facts_state_check
        CHECK (state IN ('unknown', 'open', 'closed', 'merged')),
    CONSTRAINT github_pull_facts_review_decision_check
        CHECK (review_decision IN ('unknown', 'none', 'review_required', 'approved', 'changes_requested')),
    CONSTRAINT github_pull_facts_mergeable_state_check
        CHECK (mergeable_state IN ('unknown', 'blocked', 'clean', 'dirty', 'unstable',
                                   'behind', 'draft', 'has_hooks')),
    CONSTRAINT github_pull_facts_completeness_check
        CHECK (completeness IN ('unknown', 'partial', 'complete')),
    CONSTRAINT github_pull_facts_completeness_dated_check
        CHECK (completeness = 'unknown' OR fetched_at IS NOT NULL),
    CONSTRAINT github_pull_facts_failure_class_check
        CHECK (last_error_class IN ('', 'not_found', 'forbidden',
                                    'rate_limited', 'timeout', 'canceled', 'network',
                                    'server', 'moved', 'invalid', 'no_credential',
                                    'no_target', 'unsupported_host'))
);
