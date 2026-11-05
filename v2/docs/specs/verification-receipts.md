# Verification receipts and video evidence

Status: Reviewed · Date: 2026-11-04 · Issue: #126 · Paths are relative to `v2/`.

## Problem

The owner says: "I'm struggling to see what this system is doing, what it needs from me, and what
it completed / was it verified." Today *Done* only changes the ticket's state:

- **The evidence is scattered.** Done time and the merge line are in the drawer header. Runs are
  in Details, artifacts in their own strip, and the worker's `VERIFIED` / `COULD NOT VERIFY` list
  in a History comment (`amp-execution` Step 6).
- **"Verified" is free text.** `domain.Run` has no checks or verdict, and a person's acceptance
  isn't recorded.
- **UI work can't be shown.** Artifacts are 10 MB BYTEA (`migrations/018_artifacts.sql`) and go
  through artifact review, so they can't hold video.

**Fixed when:** in a project with receipts on, the first screen of a completed ticket's drawer
shows four things: who did it, where the code is, how it was checked, and its status. Any
uploaded evidence plays or shows inline. Signal: every completed agent ticket there has a Board
badge, and Mission's Done readout counts the unverified ones.

## Goals / Non-goals

- Goal: a **completion receipt** at the top of a completed ticket's drawer. Phase 1 builds it from
  data AMP already has, with no schema change, and it works in every project.
- Goal: a **receipt per completing run**, with structured checks and a derived four-level status:
  `unverified → self_checked → independently_verified → human_accepted` (V5).
- Goal: a **per-project receipts setting**, Off / Inform / Require. The org default is Off. In
  Require mode a ticket can't be marked done below a minimum level.
- Goal: **evidence storage and playback**. The `ReceiptStore` keeps evidence on local disk,
  amp-api serves it with Range, and the drawer plays it.
- Non-goal (follow-ups, see the end): the recorder, the gate model and independent verifier, and
  the S3 backend.
- Non-goal: putting evidence in **artifacts** or in the artifact review queue (V3).
- Non-goal: the live step viewer, `check_suite` ingestion, day-report counts, a harness verifier.
- Non-goal: **receipts on human tickets.**

## The receipts setting

This amends V4 ("verification informs, never blocks"). The owner made the change on 2026-11-04.

| Mode | What happens |
|---|---|
| **Off** (org default) | No receipt rows, no evidence, no recorder. The drawer shows the Phase 1 receipt built from the comment, as it does for every project today. |
| **Inform** | Receipts and checks are recorded and evidence is stored. The status is shown. Unverified work still completes, merges and releases its dependents. |
| **Require** | Same as Inform. In addition, a ticket can't move to `completed` until its receipt reaches the minimum level. |

- **Off is the default** because live projects already exist and must not change underneath
  them, and because evidence uses storage. Nothing changes until someone turns receipts on.
- **Where it is set.** `orgs.receipts_mode` holds the org default. `projects.receipts_mode` is
  `''` (use the org's) or a mode. Org admins set the org default on the Orgs page. Project owners
  and org admins set the project value on Project settings, next to `require_merge`, through the
  same `authz.Owner` check in `service.UpdateProject`. Changes are audit-logged with before and
  after values.
- **Agents never change it.** Both settings need `p.InUI()` (`authz.RequireUI`), like turning
  plan approval off.
- **Minimum level** (Require only): `projects.receipts_min_level`. v1 offers `self_checked`;
  `independently_verified` arrives with the verifier follow-up.
- **A person's Accept always satisfies it.** `human_accepted` is above every minimum.
- **Couldn't verify waits for a person.** A Require ticket that completes below its minimum (for
  example, every check is `cant_tell` because evidence is missing) is held. It stays
  `in_progress`, the dispatch ends, and the receipt is final with `held = true`. The drawer and My
  Work show "Couldn't verify, needs you" with the reason. Accept completes the ticket. Reject,
  with a note, returns it to `backlog`. Nothing passes silently, and only that ticket and its
  dependents wait.
- **Per-ticket override.** Ticket metadata `receipts` and `receipts_min_level` follow the
  project → epic → story → ticket inheritance. An agent may only make them stricter. A weaker
  value needs a person in the web UI. The drawer shows "Requires receipt" when the effective
  mode is Require.
- **Changing modes** affects only later completions. Held tickets stay held until a person acts.
  Turning receipts Off keeps existing receipts, which still expire under retention.

## Journeys

1. **Owner checks a finished UI ticket (Inform).** The frontend worker on #412 uploaded its
   screenshots and completed with a passing `test` check and a passing `visual` check that cites
   a screenshot. On the Board, #412's card shows a blue shield and an image icon. The receipt
   reads "Done by amp-worker-frontend · claude-opus-5-5 · 2h ago", "ux/receipts @ 3f2a9c1 →
   main · PR · Merge needed" and "Self-checked 2/2", with the screenshot inline. The owner clicks
   **Accept**, and the badge reads "Accepted".
2. **Evidence is missing (the most likely failure).** A `visual` check cites a screenshot that
   never finished uploading. At finalize it becomes CAN'T TELL ("evidence missing") and shows in
   warning color. Under Inform the ticket completes. Under Require with no other passing check,
   the ticket is held: "Couldn't verify, needs you".
3. **An old ticket, an old plugin, or an Off project.** There is no receipt row. The latest
   "Work complete." comment has 3 `VERIFIED` lines and 1 `COULD NOT VERIFY`. The receipt shows
   "Self-checked (from comment) 3/4", the unverified criterion, and "Unverified", with no errors.

## Design

### Overview

```
worker                                amp-api                                  UI
------                                -------                                  --
amp_open_receipt ──────────────▶ task_receipts (draft), or {mode: off}
amp_add_evidence ──────────────▶ receipt_evidence (pending) + one-time upload URLs
PUT upload ────────────────────▶ /api/receipt-uploads/<token> ─▶ ReceiptStore.Put (sha256)
amp_complete_task(receipt_id, checks[]) ─▶ final receipt + checks ─▶ complete, or hold (Require)
person: Accept / Reject (web UI only) ─────────────────▶ checks (role human)
                                GET /api/tasks/:id/receipt ─────────▶ CompletionReceipt
                                GET /api/receipts/<key> (Range) ────▶ <video>, <img>
                                GET /api/projects/:id/receipts/summary ─▶ TaskCard badge, Mission
sweeper (hourly, advisory lock) ─▶ deletes unkept evidence past retention
```

### Phase 1 receipt (v0, client-side, every project)

`CompletionReceipt` renders at the top of the drawer for a completed agent ticket. It replaces
the header's "Done …" and merge-status lines. The drawer loads the runs once and shares them with
`MergePanel` and `MergeGateNote`.

- Line 1: "Done by `<agent_id>` · `<model>` · `<time>`", from the latest `completed` run, with a
  Closeout tag when its note starts with `closeout:`.
- Line 2: "`branch` @ `commit[:7]` → `base_branch`", the PR link, and `mergeStatusText(task)`,
  from the latest run that has them (`latestRunWith`, extracted from `MergePanel`).
- Line 3: `parseSelfCheck(comments)` reads the latest comment starting `Work complete.` and its
  `- <criterion>: VERIFIED` / `- <criterion>: COULD NOT VERIFY <why>` lines. It shows
  "Self-checked (from comment) V/(V+C)" with the status "Unverified", or "No self-check posted".
- Line 4: "N artifacts", hidden at 0.

`Run` in `ui/src/types/index.ts` gains `base_branch?: string`, which the API already returns.
`RunsPanel` shows it.

### Data model

Migration `0NN_verification_receipts.sql`, additive only. Its number is the next free one when it
merges (after 042; #125 also adds migrations).

```sql
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
```

`role = 'verifier'` is in the check enum now so the verifier follow-up needs no constraint
change. The follow-ups add their own columns (plan, verifier lease, journey and side) when they
need them. Go types go in `amp-api/internal/domain/receipt.go`.

### Verification status (derived, never stored)

`domain.DeriveStatus(r Receipt, checks []ReceiptCheck) string` is a pure function over a final
receipt. It returns the first rule that matches:

1. `human_accepted`: the latest `role=human, kind=verdict` check is `pass`.
2. `independently_verified`: the latest `role=verifier, kind=verdict` check is `pass` and its
   `checked_by` is not the receipt's `agent_id`. (Nothing writes these in v1.)
3. `unverified`, labelled "Verifier failed": the latest verifier verdict is `fail`. (Follow-up.)
4. `self_checked`: the `role=worker` checks include at least one `pass` and no `fail`. A
   `cant_tell` check doesn't count against this and is shown as a warning.
5. `unverified`: anything else, including no checks.

A later human `fail` (Reject) overrides an earlier Accept. The drawer then shows "Rejected by
`<name>`: `<note>`". A ticket's status is the status of its latest final receipt. Reopening and
completing again makes a new receipt; older ones are listed as attempts.

`meetsMinimum(status, min)` orders the ladder (`unverified < self_checked <
independently_verified < human_accepted`). In Require mode it decides whether a completion
proceeds or is held. In Inform mode it is never consulted, and `merge_state` and `require_merge`
work exactly as they do today.

### Receipt lifecycle and MCP tools

Tools go in `amp-api/internal/mcp/receipts.go`, registered in `server.go` and listed in
`catalog.go`.

- `amp_open_receipt(task_id, dispatch_id, files_changed?, summary?)` → `{receipt_id, mode}`. The
  caller must hold the ticket's current dispatch (`authz.TaskWork`). In an Off project it returns
  `{receipt_id: 0, mode: "off"}` and stores nothing. A second call for the same dispatch updates
  and returns the same draft.
- `amp_add_evidence(receipt_id, dispatch_id, commit, tree_sha, files: [{name, bytes, sha256, duration_ms?, frames?}])`
  → `{uploads: [{name, key, url, expires_at}]}`. It needs `TaskWork`, a draft receipt and the
  ticket's current dispatch. It sets the receipt's `commit_sha` / `tree_sha`. Names must be on
  the allowlist, and the server sets the content type. Files over `RECEIPT_MAX_BYTES` are refused.
  The key is `<project_id>/<task_id>/attempts/<receipt_id>/<tree_sha[:12]>/<name>`. Re-adding a
  key re-mints a `pending` row, returns an identical `stored` row with no `url`, and refuses a
  `stored` row with a different `sha256`. The `url` is `<APP_PUBLIC_URL>/api/receipt-uploads/<token>`
  (a bare path under `make dev`, prefixed with `$AMP_URL`). The token is 32 random bytes,
  base64url, single use, valid 15 minutes, and stored only as a sha256 (the relay connect-token
  scheme, in `internal/receipts/token.go`).
- `amp_complete_task` gains optional `receipt_id`, `checks[]`, `files_changed[]` and `tree_sha`;
  old callers are unchanged. `service.FinalizeReceipt(ctx, p, taskID, run, in)` is called by the
  MCP `completeTask` after it records the run, and with a nil run by `CompleteTask` (UI/REST)
  and `SetTaskStateAs`. It acts only on a real transition into `completed`. To tell, the actor
  replies gain `Changed bool` (like `ReplyMergeState.Changed`) and `service.Closeout` gains
  `Completed`. In an Off project it returns at once. Otherwise it:
  1. loads the draft (it must belong to this ticket and, for an agent, this dispatch, else a tool
     error and the ticket stays open), or adopts the newest open draft, or creates one;
  2. copies commit, branch, base branch and PR from the run (or the latest run that has them);
  3. inserts `checks[]` with `role=worker`;
  4. downgrades to `cant_tell` any `visual` check without a `stored`, non-zero-frame evidence row
     of this receipt ("evidence missing" / "no frames"), or recorded on a different `tree_sha`
     when both are set;
  5. sets `state=final`, and `kept` if the ticket is already merged;
  6. in Require mode, if `meetsMinimum` fails: sets `held`, leaves the ticket `in_progress`,
     releases the dispatch, posts "Couldn't verify, needs you: <reason>", and returns
     `{held: true, reason}` to the worker as success, so it stops instead of retrying;
  7. returns `receipt.updated` for the caller to publish (the service has no hub).

  A person's Done in the UI on a Require ticket below the minimum is held the same way, with
  Accept right there. No MCP tool writes `role=human` checks: Accept and Reject are REST only, for
  `p.InUI()`, as in artifact review.

`checks[]` item: `{criterion_ref, kind: command|test|ci|visual|manual, verdict:
pass|fail|cant_tell, command?, exit_code?, duration_ms?, note?, evidence_key?}`. Legacy `{ok}`
maps to pass / fail. A `ci` check is a worker check (CI runs the worker's own tests), so it
reaches `self_checked` at most.

### Evidence store (V2, V3)

Package `amp-api/internal/receipts`:

```go
type Store interface {
    // Put writes key once and returns the sha256 and length of the stored bytes.
    // ErrExists if the key is stored; ErrTooLarge past max.
    Put(ctx context.Context, key, contentType string, r io.Reader, max int64) (sha256 string, n int64, err error)
    // Serve writes the object to w; local uses http.ServeContent, so Range works.
    Serve(w http.ResponseWriter, r *http.Request, key string) error
    Delete(ctx context.Context, key string) error
}
func ValidateKey(key string) error // no "..", leading "/", "\\" or empty segment; [A-Za-z0-9._/-] only
func New(cfg Config) (Store, error)
```

- **`local`** is the only backend in v1. Its root is `RECEIPT_DIR`, default `data/receipts` under
  amp-api's working directory (git-ignored). `Put` writes a temp file while hashing and
  `os.Link`s it into place, which is atomic and once-only, on NFS too. `Serve` uses
  `http.ServeContent` with the sha256 as ETag.
- The interface is shaped so the S3 follow-up adds `s3.go` and a `RECEIPT_STORE` value without
  touching callers. The sweep is driven by Postgres, so retention needs no bucket rules.
- **Config** (env, read in `cmd/server/main.go`):

  | Variable | Default | What it sets |
  |---|---|---|
  | `RECEIPT_DIR` | `data/receipts` | local root |
  | `RECEIPT_MAX_BYTES` | `209715200` (200 MiB) | per-file cap |
  | `RECEIPT_RETENTION_DAYS` | `90` | how long unkept evidence lives (`<= 0` means the default) |

  If the store fails to start, amp-api logs it and keeps running. Evidence calls answer "evidence
  storage isn't configured: <why>", and visual checks then downgrade, so Require holds them for a
  person.
- **Allowlisted names:** `video.mp4` (`video/mp4`), `poster.gif` (`image/gif`), `contact.png`
  and `screenshot-<n>.png` (`image/png`), `trace.zip` (`application/zip`), `har.json` and
  `receipt.json` (`application/json`), `console.txt` and `transcript.txt` (`text/plain;
  charset=utf-8`).
- **Deploy.** Compose: amp-api gets `RECEIPT_DIR=/data/receipts` and a named volume
  `amp-receipts`. Helm: a ReadWriteMany PVC (`nfs-storage` in production, default 20Gi), so any
  number of amp-api replicas sees the same files. Both are created whatever the mode, and they
  stay empty while every project is Off.

### HTTP routes (`amp-api/internal/api/receipts.go`)

- `PUT /api/receipt-uploads/<token>`: no session, the token is the credential. It finds the
  pending row by token hash and checks expiry. It caps the body with `http.MaxBytesReader` at the
  declared size and calls `Store.Put`. If the object exists while the row is pending, it is
  replaced. A sha256 or length mismatch deletes the object and answers 422. On success the row is
  `stored`, the token is cleared, and `receipt.updated` is published. `authExceptHealth` exempts
  the prefix, like `api.GitHubHookPrefix`.
- `GET /api/receipts/<key>`: `ValidateKey`, then `authz.TaskRead` on the row's ticket, then
  `Store.Serve` (410 once expired). Headers follow `writeArtifactContent`: `nosniff`, the
  `artifactSandbox` CSP, no referrer, same-origin CORP, and `inline` (media, text) or
  `attachment` (`trace.zip`, `har.json`).
- `GET /api/tasks/:id/receipt` returns `{receipt, checks, evidence, status, held, attempts}`, or
  `{receipt: null, mode}`. `GET /api/tasks/:id/receipts` returns all of them, newest first.
- `POST /api/tasks/:id/receipt/accept {note?}` and `/reject {note}`: `p.InUI()` only, and Reject
  needs a note. Each adds a human verdict check and an audit entry, and Accept sets `kept`. On a
  held receipt, Accept completes the ticket through `CompleteTask`, and Reject returns it to
  `backlog` with the note as a comment.
- `GET /api/projects/:id/receipts/summary` returns `{mode, min_level, tasks: [{task_id, status,
  held, has_media}]}`.
- The project settings PATCH gains `receipts_mode` and `receipts_min_level`. The org default gets its own
  org-admin, web-UI-only route, `PUT /api/orgs/:id/receipts {mode}`, following
  `PUT /api/orgs/:id/remote-control` (`PATCH /api/orgs/:id` stays name-only).
- `receipt.updated` (`{task_id, receipt_id, status, held}`) joins `AllEventTypes`.

### Retention

`service.RunReceiptSweep(ctx, interval, repo.TryNamedLock)` runs hourly on one replica, like
`RunArtifactPurge`:

- It deletes stored evidence of receipts with `kept = false` and `created_at` older than
  `RECEIPT_RETENTION_DAYS`, then marks the rows `expired`. Metadata stays, so the drawer can say
  "evidence expired".
- It deletes `pending` rows whose upload expired more than a day ago, removing any object first.

`kept` is set for every receipt of a ticket when its merge state becomes `merged`
(`service.setMergeState`, on `reply.Changed` to `MergeMerged`) and when a person Accepts. Kept
evidence lives until the ticket is purged. Receipts have no FK to `tasks`, so `RunTrashPurge`
deletes the evidence and receipts of purged tickets before `PurgeTrash`. The two `ResetProject`
callers (`rest.go`, `mcp/server.go`) do the same for the project's receipts first.

### Worker changes in v1

Plugin sources are edited in `.opencode/` and rebuilt (`plugins/` and `harness/assets/` are
generated and drift-checked in CI).

- `amp-execution` Step 6: call `amp_open_receipt`. If `mode` is `off`, continue exactly as
  today. Otherwise pass `receipt_id` and `checks[]` (one per criterion) to `amp_complete_task`. If
  the result says `held`, stop and say why in the closing comment. The `VERIFIED` comment block
  stays for one release as a display fallback, then the template drops it.
- `amp-worker-frontend`: the screenshots it already takes for self-comparison are uploaded with
  `amp_add_evidence` as `screenshot-<n>.png`, and cited by its `visual` checks. This gives v1 real
  evidence before the recorder exists.

### UI

- `lib/receipt.ts`: `parseSelfCheck`, `latestRunWith`, `summarizeChecks`, and `STATUS_LABEL` /
  `STATUS_TONE`: `--ink-3` for unverified, `--st-prog` for self-checked, `--teal` for
  independently verified, and `--success-ink` for accepted.
- `receipt/CompletionReceipt.tsx`: shows the server receipt, or the Phase 1 view labelled "from
  comment". Below line 4 it lists the checks by role, the evidence, Accept / Reject (`canWork`, a
  person), and "N earlier attempts". A held receipt leads with "Couldn't verify, needs you", the
  reason, and Accept / Reject, and it also renders on the `in_progress` ticket. It reloads on
  `receipt.updated`.
- `receipt/EvidencePlayer.tsx`: `<video controls preload="metadata">` with poster, `<img>`,
  download links, and "Evidence expired on <date>".
- `receipt/VerificationBadge.tsx`: `Shield` / `ShieldCheck` colored by status, `Image` when
  there is media, and an amber `ShieldAlert` when held. `TaskCard` shows it and adds it to the
  `aria-label`. `Board` loads the summary once and again on `receipt.updated`. The drawer header
  shows "Requires receipt" when the effective mode is Require.
- `Mission`: the Done readout's sub reads "N unverified". Held tickets count as waiting on you.
  `MyWork`: held tickets appear as asks ("Couldn't verify #N: <reason>").
- `ProjectSettings`: a "Verification receipts" section with Use org default (shows which) / Off /
  Inform / Require, plus the minimum level under Require. It is read-only for leads. Help text:
  "Off: nothing is recorded or stored. Inform: receipts are shown, nothing waits. Require: tickets
  wait for a receipt or your Accept." `Orgs`: the org default, for org admins.

## Alternatives

- **Evidence as an artifact kind.** Rejected: artifacts are 10 MB BYTEA and go through artifact
  review (V3).
- **Uploads over authenticated REST.** Rejected: plugin workers reach AMP over MCP and have no
  token in the shell.
- **Receipts always on.** Rejected by the owner (D8).
- **Require with no human path.** Rejected: a broken store would stop a project (D10).
- **`verification_status` on `tasks`.** Rejected: it would race the task actors. The status is
  derived (D21).

## Risks

- **Stored XSS through evidence.** The content type comes from the allowlisted name. Responses
  carry `nosniff` and the sandbox CSP, downloads are `attachment`, and CORP is same-origin.
- **The unauthenticated upload route.** Tokens are 256-bit, single use, stored as hashes, and
  expire in 15 minutes. The size is fixed at mint, and the sha256 must match. It is the only new
  exempt prefix. Helm gets a dedicated Envoy route (300 s, ext_authz off for that prefix only).
  Compose's nginx gets `client_max_body_size 210m` there.
- **Require holds more than expected**, for example with an old plugin that sends no checks.
  Before saving Require, Project settings warns how many tickets from the last 7 days would have
  been held. Held tickets show in My Work, and Accept clears them.
- **The disk fills.** Receipts are off by default. There is a per-file cap and a 90-day sweep.
  The KB has a sizing note (about 5–20 MB per recorded journey) and the PVC size is configurable.
- **Multi-replica storage and permissions.** The PVC is RWX on `nfs-storage`, writes use
  `os.Link` once, and the sweep takes an advisory lock. The directory is owned by uid 1000, with
  `fsGroup: 1000` in Helm.
- **Evidence backup.** The volume is outside Postgres, so it is covered by `nfs-storage` backups.
  Database backups are #145.
- **Migration number collision** with #125. Take the next free number when merging.
- **Rollout and rollback.** The migration is additive and every project starts Off. Rollback is
  the previous image.

## Decisions & Assumptions

| ID | Decision or assumption | Why | By |
|----|------------------------|-----|----|
| D1 | The recorder follows Piquette/SPF (see Follow-ups). v1's evidence names and `tree_sha` binding are built to fit it. | Owner decision V1 | human |
| D2 | Storage is local by default and S3 optional, behind one `ReceiptStore`. v1 ships local; S3 is a follow-up. | Owner decision V2 | human |
| D3 | Evidence is not an artifact. Postgres holds metadata only. | Owner decision V3 | human |
| D4 | **Amends V4:** receipts are a per-project setting, Off / Inform / Require. Inform never blocks. Require holds a ticket below its minimum until a person Accepts. | Owner, 2026-11-04 | human |
| D5 | Status ladder: unverified → self_checked → independently_verified → human_accepted. | Owner decision V5 | human |
| D6 | A cheap local model (jev, clef or nimble on Ollama) may only add checks, and a failure gives the full check. It is optional and off by default (`VERIFY_GATE_MODEL`), with the default model chosen by a bake-off on 20 past tickets. Follow-up. | V6; owner, 2026-11-04 | human |
| D7 | Related work ships as one PR. | Owner process constraint | human |
| D8 | The org default is **Off**, and a project can override it. | Live projects must not be disrupted; storage scale | human |
| D9 | Only project owners and org admins change the setting, in the web UI. Agents never do. | Same as `require_merge` | human |
| D10 | Accept always satisfies Require. A failed or impossible check holds the ticket for a person. It never passes silently and never stops the project. | Owner, 2026-11-04 | human |
| D11 | Per-ticket overrides may only raise the requirement, unless a person in the web UI lowers it. | Owner, 2026-11-04 | human |
| D12 | v1 is Phases 1–3. The recorder, the gate model and verifier, and the S3 backend are follow-ups. | Owner, 2026-11-04 | human |
| D13 | Unkept attempts expire after 90 days. A ticket's receipts are kept forever once it merges or a person Accepts. | Owner, 2026-11-04 | human |
| D14 | A CI pass is a worker check: `self_checked` at most. | Owner, 2026-11-04 | human |
| D15 | No receipts on human tickets in this change. | Owner, 2026-11-04 | human |
| D16 | The `VERIFIED` comment stays as a display fallback for one release. | Owner, 2026-11-04 | human |
| D17 | No in-cluster S3 now (future: #144). Database backups to an S3-compatible store are tracked in #145. The S3 backend can reuse either store by config. | Owner, 2026-11-04 | human |
| D18 | No verifier token budget. | Owner, 2026-11-04 | human |
| D19 | The worker writes and commits `.amp/journeys/<task_id>.json`, and the verifier adds an unhappy path. Follow-up. | Owner, 2026-11-04 | human |
| D20 | There is one receipt per completing run. It opens as a draft before completion so evidence can upload first. | The run doesn't exist at upload time | agent |
| D21 | The status is derived on read. Only `held` is stored. | One source of truth; no race with task actors | agent |
| D22 | Accept / Reject are human verdict checks, REST and `InUI()` only. Reject needs a note. | Matches artifact review | agent |
| D23 | Uploads use one-time URLs minted over MCP and proxied by amp-api, with the sha256 checked. | Works from every harness | agent |
| D24 | Content types come from an allowlist of names. Retention is a Postgres-driven sweep. | XSS safety; one mechanism for any backend | agent |
| D25 | A receipt is finalized only on a real transition into `completed`. A UI completion adopts the newest open draft. | Completion is idempotent today | agent |
| D26 | A held ticket stays `in_progress` and its dispatch is released. Reject returns it to `backlog`. | No new task state; rework uses normal dispatch | agent |
| D27 | The v1 minimum level is `self_checked`. `independently_verified` arrives with the verifier. | Only Accept could reach it in v1 | agent |
| D28 | Old completions, old plugins and Off projects show the comment parse, labelled "from comment". | Honest display, no backfill | agent |

## Open questions

None open. The owner answered every question on 2026-11-04 (D4, D6, D8–D19). Related issues:
in-cluster store #144, database backups #145.

## Implementation plan

Each phase ends with the Verification commands passing. All three ship in one PR (D7).

### Phase 1: receipt v0 in the drawer (UI only, no schema)

- `ui/src/lib/receipt.ts` (create): `parseSelfCheck` and `latestRunWith`, tested for VERIFIED,
  COULD NOT VERIFY with `— why` and `: why`, the latest comment winning, none found, malformed
  lines, and CRLF.
- `components/receipt/CompletionReceipt.tsx` (create), with tests for "Self-checked (from
  comment) 3/4", "No self-check posted", branch/sha/PR from the latest run, the Closeout tag,
  hidden on human tickets, and the merge status shown once.
- `TaskDrawer.tsx` (modify): render the receipt above `TicketArtifacts` for completed agent
  tickets, drop the header's Done and merge lines then, and load the runs once.
- `RunsPanel.tsx`, `types/index.ts` (modify): `base_branch`.

### Phase 2: receipt model, checks, status, the setting, Accept

- The migration (create), with the columns and tables in Data model.
- `domain/receipt.go` (create): types, `DeriveStatus`, `meetsMinimum`, `ParseChecks` (validates,
  maps legacy `ok`, caps at 100 checks and 2 KB per note), with one test per status rule.
  `domain/models.go` (modify): `EventReceiptUpdated` and the settings fields.
- `repository/receipts.go` (create): drafts, finalize, checks, latest, list, summary, keep, held.
  Project and org updates (modify): the new columns.
- `service/receipts.go` (create): `EffectiveMode` (ticket metadata → project → org), open,
  finalize with the hold, accept, reject, summary, and audit.
- `service.UpdateProject`, `service/orgs.go` and the metadata write path (modify): the settings
  with `RequireUI`, the audit before/after, and refusing a weaker override from an agent.
- `service/merge.go`, `approval.go`, `actor/` (modify): `KeepReceipts` on merge, `Changed` on
  the actor replies, `Closeout.Completed`, and finalize-and-hold in `CompleteTask` and
  `SetTaskStateAs`.
- `mcp/receipts.go` (create), `mcp/runs.go`, `server.go`, `catalog.go` (modify):
  `amp_open_receipt`, the new `amp_complete_task` fields, and the `held` result.
- `api/receipts.go` (create) and `rest.go` (modify): the receipt routes and settings fields.
- `cmd/server/receipts_test.go` (create, integration):
  - Off (the default): nothing is recorded and completion is unchanged.
  - Inform: checks give `self_checked`; no checks give `unverified`; completing twice leaves one
    receipt; a UI completion adopts the draft; another ticket's `receipt_id` is refused.
  - Require: below the minimum is held (still `in_progress`, comment posted, dependents waiting);
    Accept completes it as `human_accepted`; Reject returns it to `backlog`; meeting the minimum
    completes normally.
  - An agent can't change the setting or weaken a ticket override, but can strengthen one. A
    lead's change is 403.
  - An agent's Accept over REST is 403; Reject without a note is 400; Accept and merge set `kept`.
  - Old `amp_complete_task` calls are unchanged outside Require.
- UI (modify or create): the client calls and types, `CompletionReceipt` (server path, held
  state, Accept / Reject, attempts), `VerificationBadge`, `TaskCard`, `Board`, `Mission`,
  `MyWork`, `ProjectSettings` and `Orgs`. Tests cover the badges, Accept / Reject only for
  `canWork`, Reject needing a note, the read-only lead view, and the Require warning count.
- `.opencode/skills/amp-execution/SKILL.md` (modify), then rebuild the plugins and harness
  assets. `kb/working/verification-receipts.md` (create).

### Phase 3: local evidence store, serving, retention, player

- `internal/receipts/` (create): `store.go`, `local.go`, `key.go`, `token.go`, with tests for
  the sha256, `ErrExists`, `ErrTooLarge`, Range 206, and `ValidateKey`.
- `amp_add_evidence`; evidence rows; token minting; `StoreUpload`; `RunReceiptSweep`; the
  visual-check downgrade; the upload and serve routes; the `main.go` env, store, sweep and
  exemption.
- `cmd/server/receipts_test.go` (modify):
  - Upload, then a Range GET plays. A sha mismatch is 422 and stores nothing. A reused token is
    404 and an expired one 410. A non-member GET is 403.
  - The sweep expires unkept evidence and leaves kept evidence. Evidence never lists as an
    artifact.
  - Missing or foreign evidence downgrades a visual check, as does a `tree_sha` mismatch (but not
    a missing `tree_sha`).
  - Upload retries work. Another `dispatch_id` is refused.
  - Purge deletes receipts and evidence.
  - With no store, Require holds a ticket whose only check is visual.
- Trash purge and both `ResetProject` callers (modify): cleanup.
- `amp-api/Dockerfile`, `docker-compose.yml`, `.gitignore`, `ui/nginx.conf` (modify).
- Helm: `ampApi.receipts` values (20Gi, ReadWriteMany, 90 days, 200 MiB), `nfs-storage` in
  production, the PVC, mount, env and `fsGroup 1000`, and the Envoy upload route with ext_authz
  off.
- `EvidencePlayer.tsx` (create) with tests.
- `amp-worker-frontend` uploads its screenshots (then rebuild).
- `scripts/seed-sample-receipt.sh` (create): sets a local project to Inform, makes a 3 s
  `testsrc` clip, and opens, uploads and completes a receipt.
- `kb/operations/where-amp-keeps-data.md` (modify).

## Follow-ups (not in this change)

- **Recorder (`amp-verify` skill).** A bash `record.sh` that follows Piquette/SPF exactly (D1):
  agent-browser recording on clean worktrees of the base and head revisions, loopback-only ttyd
  for CLI journeys, ffmpeg MP4 / GIF poster / contact sheet, an ffprobe frame check, and a
  transcript per command. The verdict is PASS only if every head journey passes, at least one is
  an unhappy path, and some journey fails on base. Journeys live in `.amp/journeys/<task_id>.json`,
  written by the worker (D19). It adds `side` and `journey` to evidence. Reference:
  https://github.com/seesharpguy/super-portable-software-factory.
- **Gate model and independent verifier.** A heuristic plan (`self|verifier|video`) from the
  changed paths. An optional local model (jev, clef or nimble on Ollama, `VERIFY_GATE_MODEL`) can
  only raise the plan, and falls back to the full check on error (D6). Metadata `verification`
  raises the plan; `verification_max` lowers it only when a person sets it (D11). A plugin
  `amp-verifier` agent gets a brief without comments, checks out the commit, re-runs the
  journeys plus its own unhappy path, and records a verdict that can reach
  `independently_verified`. A verifier `fail` creates a "Fixes #N" ticket. Unlocks
  `independently_verified` as a Require minimum.
- **Optional S3 backend.** `s3.go` behind the same `Store` with `RECEIPT_STORE=s3` (R2, S3,
  MinIO, Garage). Short-lived presigned GETs minted on view, and a media origin added to the CSP.
  Production would point it at the store from #144 or #145; until then production uses the local
  PVC.

## Verification

Run after every phase, from `v2/`:

```sh
(cd ui && npx tsc -b && npm run lint && npx vitest run)
(cd amp-api && go vet ./... && go test ./... &&
  TEST_DATABASE_URL=postgres://amp:amp@localhost:5432/amp_test?sslmode=disable go test ./cmd/server/ -run 'Receipt|Complete|Catalog|Merge|Settings')
```

From Phase 2: rebuild the plugins and harness assets, then `git diff --exit-code plugins/` and
`scripts/build-harness-assets.sh --check`. `cd harness && cargo test` stays green (no Rust
changes).

Manual acceptance (`make dev`):

- In a fresh project, the setting shows "Use org default (Off)". Completing a ticket records no
  receipt, and the drawer shows the "from comment" receipt.
- `scripts/seed-sample-receipt.sh <task_id>` stores a clip under `amp-api/data/receipts/`. It
  plays in the drawer, and seeking returns `206 Partial Content`.
- Under Require, a ticket completed with no checks stays `in_progress` as "Couldn't verify, needs
  you", in the drawer and in My Work. Accept completes it, and its dependents start.
- After backdating a receipt and running a sweep, unkept evidence shows "expired" and merged
  evidence still plays. Artifacts and the review queue list no evidence.
- Old `amp_complete_task` / `amp_report_run` calls behave as before in Off and Inform projects.
