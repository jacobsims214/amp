-- Decision docs (#125, v2/docs/specs/human-decision-docs.md §3): a judgment
-- ticket opens as a markdown artifact an agent drafted from its ask.
--
-- Everything here is additive: new columns are nullable or defaulted, so old
-- clients and existing artifacts are unchanged.
--
-- artifacts.role                 'decision' for a ticket's decision doc; NULL otherwise.
-- artifact_versions.author_kind  who wrote a version, fixed by its write path:
--                                the rendered v1 is 'agent', an edit in AMP is 'person'.
-- artifact_versions.decision_ask a decision doc version's options:
--                                {question, options: AskOption[], recommended}.
-- decision_docs                  one row per ticket with a doc, and (once recorded)
--                                what was decided on which version.
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS role TEXT;
ALTER TABLE artifact_versions ADD COLUMN IF NOT EXISTS author_kind TEXT NOT NULL DEFAULT 'person'
    CHECK (author_kind IN ('person','agent'));
ALTER TABLE artifact_versions ADD COLUMN IF NOT EXISTS decision_ask JSONB; -- {question, options: AskOption[], recommended}; decision docs only
CREATE TABLE IF NOT EXISTS decision_docs (
    task_id              INT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    artifact_id          INT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    decided_version      INT,
    chosen_option        INT,          -- index into vN's decision_ask.options; NULL = "something else"
    decision_text        TEXT,
    decided_by           INT REFERENCES users(id) ON DELETE SET NULL,
    recording_started_at TIMESTAMPTZ,  -- Record decision's claim
    run_id               BIGINT REFERENCES task_runs(id) ON DELETE SET NULL, -- the human run Record decision wrote
    decided_at           TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS decision_docs_artifact_idx ON decision_docs (artifact_id);
