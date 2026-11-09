-- Verification receipts (#126, v2/docs/specs/verification-receipts.md).
--
-- orgs.receipts_mode          the org's default: off (the default), inform
--                             or require. Nothing changes for any project
--                             until someone turns receipts on.
-- projects.receipts_mode      '' (use the org's) or a mode.
-- projects.receipts_min_level what a Require project's tickets must reach
--                             before they complete (v1: self_checked).
--
-- task_receipts               one per completing run: a draft opened before
--                             completion (so evidence can upload first),
--                             final once the ticket completes or is held.
--                             No FK to tasks, like task_runs: it outlives the
--                             ticket. The verification status is derived on
--                             read from the checks; only held is stored.
-- receipt_checks              the checks a worker (or later a verifier) ran,
--                             and a person's Accept / Reject verdicts.
-- receipt_evidence            evidence metadata only; the bytes live in the
--                             receipt store, never in Postgres or artifacts.
--
-- Additive only; idempotent.

ALTER TABLE orgs     ADD COLUMN IF NOT EXISTS receipts_mode TEXT NOT NULL DEFAULT 'off'
  CHECK (receipts_mode IN ('off','inform','require'));
ALTER TABLE projects ADD COLUMN IF NOT EXISTS receipts_mode TEXT NOT NULL DEFAULT ''
  CHECK (receipts_mode IN ('','off','inform','require'));          -- '' = the org's mode
ALTER TABLE projects ADD COLUMN IF NOT EXISTS receipts_min_level TEXT NOT NULL DEFAULT 'self_checked'
  CHECK (receipts_min_level IN ('self_checked','independently_verified'));

CREATE TABLE IF NOT EXISTS task_receipts (
  id            BIGSERIAL PRIMARY KEY,
  task_id       INT NOT NULL,                 -- no FK, like task_runs: outlives the ticket
  project_id    INT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  dispatch_id   TEXT NOT NULL DEFAULT '',
  run_id        BIGINT,                       -- NULL for UI/REST completions
  state         TEXT NOT NULL DEFAULT 'draft' CHECK (state IN ('draft','final')),
  mode          TEXT NOT NULL DEFAULT 'inform' CHECK (mode IN ('inform','require')),  -- effective mode at open
  held          BOOLEAN NOT NULL DEFAULT FALSE,  -- Require: completion waiting for a person
  agent_id      TEXT NOT NULL DEFAULT '',
  summary       TEXT NOT NULL DEFAULT '',
  files_changed JSONB NOT NULL DEFAULT '[]',  -- [{path, added, removed}]
  commit_sha    TEXT NOT NULL DEFAULT '',
  tree_sha      TEXT NOT NULL DEFAULT '',     -- bound to revision (V1)
  branch        TEXT NOT NULL DEFAULT '',
  base_branch   TEXT NOT NULL DEFAULT '',
  pr_url        TEXT NOT NULL DEFAULT '',
  kept          BOOLEAN NOT NULL DEFAULT FALSE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  finalized_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_task_receipts_task ON task_receipts(task_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_task_receipts_project ON task_receipts(project_id) WHERE state = 'final';
CREATE UNIQUE INDEX IF NOT EXISTS uq_task_receipts_draft ON task_receipts(task_id, dispatch_id)
  WHERE state = 'draft' AND dispatch_id <> '';

CREATE TABLE IF NOT EXISTS receipt_checks (
  id            BIGSERIAL PRIMARY KEY,
  receipt_id    BIGINT NOT NULL REFERENCES task_receipts(id) ON DELETE CASCADE,
  role          TEXT NOT NULL CHECK (role IN ('worker','verifier','human')),
  kind          TEXT NOT NULL CHECK (kind IN ('command','test','ci','visual','manual','verdict')),
  verdict       TEXT NOT NULL CHECK (verdict IN ('pass','fail','cant_tell')),
  criterion_ref TEXT NOT NULL DEFAULT '',     -- 1-based criterion index ("2"), else free text
  command       TEXT NOT NULL DEFAULT '',
  exit_code     INT,
  duration_ms   INT,
  note          TEXT NOT NULL DEFAULT '',
  evidence_key  TEXT NOT NULL DEFAULT '',
  checked_by    TEXT NOT NULL DEFAULT '',     -- agent id, or the person's name
  user_id       INT REFERENCES users(id) ON DELETE SET NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_receipt_checks_receipt ON receipt_checks(receipt_id);

CREATE TABLE IF NOT EXISTS receipt_evidence (
  id            BIGSERIAL PRIMARY KEY,
  receipt_id    BIGINT NOT NULL REFERENCES task_receipts(id) ON DELETE CASCADE,
  project_id    INT NOT NULL,
  key           TEXT NOT NULL UNIQUE,
  name          TEXT NOT NULL,
  tree_sha      TEXT NOT NULL DEFAULT '',     -- the revision this file was recorded on
  content_type  TEXT NOT NULL,
  bytes         BIGINT NOT NULL DEFAULT 0,
  sha256        TEXT NOT NULL DEFAULT '',     -- declared at add, checked against stored bytes
  duration_ms   INT,
  frames        INT,
  status        TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','stored','expired')),
  upload_token_hash TEXT NOT NULL DEFAULT '',
  upload_expires_at TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  stored_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_receipt_evidence_receipt ON receipt_evidence(receipt_id);
CREATE INDEX IF NOT EXISTS idx_receipt_evidence_token ON receipt_evidence(upload_token_hash) WHERE status = 'pending';
