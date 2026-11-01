-- Merge-needed signal (AMP #2475, KB decisions/human-work-ux.md §4).
--
-- A ticket that completes with a run carrying a PR (or a pushed branch)
-- records merge_required, and its merge_state is 'needed' until the merge is
-- recorded (GitHub webhook, CI's amp_report_merged, or Mark merged in the UI:
-- the first report wins and is kept in merge_source/merged_at). While a
-- dependency is 'needed' (or 'abandoned': its PR closed without merging),
-- the tickets that depend on it wait, unless its project turns
-- require_merge off.
--
-- Additive only. Every existing ticket starts at merge_state 'none', so
-- nothing completed before this migration is gated. require_merge_since is
-- when the project's require_merge last went on (this migration for existing
-- projects): a PR reported late on a ticket completed before it doesn't gate
-- anything either. Idempotent.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS merge_required BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS merge_state TEXT NOT NULL DEFAULT 'none';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS merged_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS merge_source TEXT NOT NULL DEFAULT '';

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tasks_merge_state_check') THEN
        ALTER TABLE tasks ADD CONSTRAINT tasks_merge_state_check
            CHECK (merge_state IN ('none', 'needed', 'merged', 'abandoned'));
    END IF;
END $$;

-- The merge queue and dependency checks look for done tickets still waiting.
CREATE INDEX IF NOT EXISTS idx_tasks_merge_waiting ON tasks(project_id)
    WHERE merge_state IN ('needed', 'abandoned');

ALTER TABLE projects ADD COLUMN IF NOT EXISTS require_merge BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS require_merge_since TIMESTAMPTZ NOT NULL DEFAULT NOW();
