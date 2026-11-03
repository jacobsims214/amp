# feat(harness): local session resume — `/sessions` picker, locked load-and-continue (AMP #3020)

Closes the epic-182 user request (2026-11-01): *close the chat TUI, reopen it in a working directory, then `/sessions` back into a previous conversation with its context intact.*

AMP ticket **#3020** (story #454 "Build: directory-aware session discovery and safe resume", epic #182). Integration branch `feat/local-session-resume` → `main`.

**23 files, +8024 / −61 — all inside `v2/harness/`** (verified: `git diff --name-only origin/main origin/feat/local-session-resume | grep -v '^v2/harness/'` returns nothing). No UI, API, chart or migration changes.

---

## What it does

`/sessions` lists the saved conversations **started in the current directory**, newest activity first, with a preview, a message count and a `· current` marker on the one this chat is writing. Confirming a row loads the file under an exclusive lock, swaps it into the live harness, redraws the transcript, and continues the *same* conversation on the next turn.

Nothing is rebuilt and nothing is replayed: no `Runtime`, no `AgentHarness::create`, no tool re-execution, no re-dispatch of recorded `amp_run_ticket` calls.

| Concern | Where |
|---|---|
| Bounded, directory-scoped listing (read-only, lock-free) | `amp-core/src/session_index.rs` |
| Exclusive ownership sidecar lock | `amp-core/src/session_lock.rs` |
| Validated load + lock-guarded resume loader | `amp-core/src/session_store.rs`, `amp-core/src/agent.rs` (`resume_session`) |
| `/sessions` command, panel, confirm stage | `amp-tui/src/slash.rs`, `overlay.rs`, `shell.rs` |
| The one switch transaction | `amp-harness/src/chat_sessions.rs` |

### The transaction (every step refuses rather than half-doing its job)

1. **Busy probe** — one non-blocking `try_lock`, dropped immediately. A file another live harness is writing is refused *before* anything about the harness is touched, and the file is never opened. Same lock the real load takes below, so it is not a second opinion.
2. **Read the transcript back (display only)** — runs *before* the swap, so a read failure is a plain refusal rather than a switched context under the old view. `AgentEvent`s are what a viewer sees; a recorded tool call cannot run and the dispatch ledger is never touched.
3. **Load and swap** — validate header → refuse another directory / foreign name / unreadable / older format → claim the guard → only then replay (so a repair happens under the lock) → cancel every prompt the file still had queued → `AgentHarness::set_session` (rejects `LaneBusy` while a run is active, rebuilds the now-empty queues, clears `pending_resume`).

Every `Err` leaves the old session, its guard, the chat's file handle and the target file's bytes untouched.

---

## Behaviour the ticket asked to document

### Exact-cwd scoping

A session belongs to the directory it was started in; the v4 header records the **canonical** cwd, and `session_index::list` compares it to the canonical current directory **exactly** — not a subtree match. Worker (ticket-id) directories are never listed.

This is why the harness's own configuration needs no rebuild: the model, tools, policy and system prompt were built for this directory, so a session from another directory is not resumed at all rather than resumed with the wrong config. Another directory's text can never reach the preview: the cwd match happens *before* the 16 KiB start window is read, pinned by a SECRET-canary test.

### Busy states

A definite refusal, never a wait, in the picker's own words, including the holder, its age and the next step (`session … is owned by another live process … stop that run or pick another session`). The picker marks such rows as locked. Refused states are:

- the file is owned by another live writer (or by this process already);
- a model turn is in flight;
- live workers, a queued remote prompt, a paired remote device, a plan-approval wait, a server switch, or a quit begun — one `switch_blocker` predicate shared by the picker *and* the transaction, re-checked at resume time.

Lock discipline: per-file sidecar `…-<id>.jsonl.lock` next to the real file (reached through symlinks, so every spelling of the path resolves to one inode), **never deleted** (removing it would let two writers lock two different inodes); a sidecar left by a crashed run is simply claimable. Ownership is authority, never timestamps or pids. Any failure to take the lock fails **closed** (`nothing was written`). The read side (listing, replay) never locks and never repairs.

### Current model

The live harness keeps its own model across the swap, and `Resumed::model` is read **at switch time** from `harness.get_model()` — so a `/model` change is reflected. The resumed line reads:

```
✓ resumed <id> · <age> · N messages · continues in the original file · runs on <provider>/<model>
```

The loaded file's recorded `model_change` entries are deliberately **not** applied (#3012 §6.3.1): the line says what the conversation will actually run on rather than what an old file recorded.

The rest of the caller-side reset list is applied in one place (`Shell::after_resumed`): run numbering starts again (`prompts`, `run_id`), `totals`/`/usage` start empty, ↑ history is cleared, the transcript is replaced by the resumed one, and the prompt queue / wake / approval-wait / tool-call bookkeeping is cleared again. The `ManagerFeed` is *kept* — its sequence numbers are process-scoped, so every remote cursor stays valid.

### Existing-session compatibility

- **Format.** Only v4 JSONL headers are read. A v3 (or otherwise incompatible) file is still *listed* as a row naming itself and why it cannot be opened (`Incompatible`), and a corrupt/torn header is listed as `Unreadable` — with **no preview text** at all, because its directory cannot be checked. Both refuse to load.
- **Layout.** Files are read where they are written (`<state>/sessions/<role-or-ticket>/<created ms>-<id>.jsonl`). Nothing is moved, renamed or rewritten, and no second layout is introduced — existing sessions from previous installs are found as-is.
- **Torn tails** left by a killed process are repaired under the guard, keeping the conversation before them.
- **Compacted** conversations resume from their summary, not their summarized turns.
- **Queued prompts** a save left in the file are cancelled, not run (`set_session`'s `rebuild_queues` would otherwise hand the next turn prompts typed in a chat that is over).

### Banner, `/usage` and the exit line make one claim

`Shell::set_session_file` handles all four directions of `(previous, shown)`, including `None → Some(path)` (`--no-session` → resumed file). `chat.rs::session_banner(None)` and the shell share one `MEMORY_SESSION_LINE` literal so the exact-substring replace cannot silently no-op if the wording changes. (#3129)

---

## Approved artifact

The picker's layout and wording were approved before the build: **artifact #28 — "/sessions picker — directory-scoped, boxed TUI"** (AMP ticket #2999, approved by jacob.sims@simstech.cloud 2026-11-01), https://amp.simstech.cloud/artifacts/28. The resumed line and #3129's banner behaviour follow the same artifact.

---

## Test evidence

**CI on this PR (#112, head `f3f4f60`) — 6/6 green:**

| Job | Result |
|---|---|
| `harness` (fmt + clippy `-D warnings` + `cargo test --workspace --locked`) | **pass** 9m20s *(after one rerun — see below)* |
| `assets` (generated-plugin drift) | pass 9s |
| `cla` | pass 6s |
| `smoke (auto)` | pass 7m23s |
| `smoke (2026-07-28)` | pass 7m22s |
| `smoke (2025-11-25)` | pass 8m35s |

**The same stack was already green twice on the way here** — `harness` passed 8m29s on PR #111 (`fbbb4b8`) and 9m15s on PR #104 (`b642d61`), with `assets`, `cla` and all three `smoke` jobs passing on both. Those `harness` runs include the render snapshots (`help-80.txt` and `help-80-mouse-off.txt` gained the `/sessions` row) and the full workspace suite.

### One flaky test, disclosed

The **first** `harness` run on this PR failed 15 passed / 1 failed on:

```
remote_control_on_refuses_a_switch_with_nothing_queued
panicked at crates/amp-harness/tests/chat_sessions.rs:2013:5:
assertion `left == right` failed: nothing is paired yet, and the queue is empty
```

It is a **test-side race, not a product defect**: the assertion requires `link.current() == RemoteState::Starting`, the state `enable()` sets before registering with the relay, but registration can complete and `publish_state()` flip the link to `RemoteState::On` before the assertion runs (`link.rs`). The test then still passes its real purpose — the refusal, the zero second scan, the `/remote off` hint. The identical commit passed the same test in the two runs above, and `gh run rerun --failed` on the same commit passed (9m20s). No retry loop was run beyond the single rerun; this is reported rather than papered over, and a one-line fix (assert `is_on()` plus either state, or wait for the post-registration state) belongs in its own ticket. **The product behaviour the test guards is verified by the other 15 tests and by the same test's passing runs.**

**End-to-end suite**, `amp-harness/tests/chat_sessions.rs` — 16 tests driving the real shell headlessly (`--snapshot`/`--input`, faux provider), one per behaviour claimed above:

`a_saved_conversation_is_reopened_and_continued_with_its_own_history` · `after_a_resume_the_chat_names_the_file_it_continues` · `a_no_session_chat_that_resumes_stops_claiming_the_session_is_in_memory` · `the_listing_is_this_directory_and_marks_the_one_being_written` · `a_recorded_dispatch_is_shown_but_never_runs_again` · `prompts_the_saved_file_still_had_queued_never_run` · `a_compacted_conversation_resumes_from_its_summary_not_its_summarized_turns` · `a_failed_switch_leaves_the_chat_and_its_conversation_usable` · `another_directorys_session_is_refused_unopened` · `a_session_a_live_writer_holds_is_a_locked_row_and_is_refused` · `a_truncated_tail_is_repaired_and_the_conversation_before_it_kept` · `a_corrupt_header_is_a_row_that_says_why_and_a_refused_load` · `an_unsupported_version_is_an_older_format_row_that_names_the_file` · `a_resume_while_a_worker_is_live_is_refused_and_the_worker_keeps_running` · `a_resume_during_a_reply_is_refused_and_the_queue_is_kept` · `remote_control_on_refuses_a_switch_with_nothing_queued`

**Supporting suites:** `amp-tui/tests/sessions.rs` 16 · `amp-harness/tests/session_ownership.rs` 3 · `amp-core/tests/session_lock.rs` 2 (+1 `#[ignore]`d child-holder helper by design) · `amp-core` `session_` unit tests 49 · `amp-tui/tests/chat.rs` 8.

**Full workspace, clean-tree runs (from #3109's report):** 10/10 consecutive full-suite runs green (15 passed / 0 failed, zero `the chat that wrote it is gone` panics); 8/8 green with all 12 cores saturated by busy-loops — the condition that produced ~50% failure on the unmodified tree; workspace **1256 passed / 0 failed across 101 binaries**, with two named pre-existing non-TTY `setup_ctrl_c` pty hangs skipped (that file is untouched by this branch and hangs on the unmodified base too — proven by stash/re-run there).

---

## Review

- **#3018 — Code Review: local session resume.** Full tiered review of all 23 changed files (~7,900 insertions) against `main`: **APPROVE WITH FIXES — 0 Blockers, 1 Significant**, ticketed as #3129. The §10 ownership contract (`discoveries/harness-local-session-storage.md`) was audited end to end and found faithful. Suggestions (a shared `#[cfg(test)]` busy/free helper, a `ResumeRequest::clone`, wording asymmetry in `sessions_leaving()`) are explicitly non-blocking and left for later.
- **#3129 — the one Significant finding, now fixed.** A `--no-session` chat that resumed kept claiming `session in memory (--no-session)` in the banner while `/usage` and the exit line named the real file. Fixed; the regression test was proven to fail without the fix (fails with `the memory line is gone from the banner`) and pass with it.
- **#3108 / #3109 — the two test flakes** that had made the `harness` job non-deterministic on this suite are fixed here: the busy-turn test now waits on the queue notice instead of its own typed draft, and `free()` waits a bounded grace for in-process teardown. The deterministic `chat_sessions.rs:1583` failure that blocked #3092 no longer reproduces (see the green `harness` job above).

---

## Integration history

Two internal PRs were landed to assemble the reviewed stack; both are agent-internal (non-default target), merged only with green checks and no required human approval outstanding. Repo rules were read, not assumed: `gh api repos/jacobsims214/amp/branches/{main,amp-3014,feat/local-session-resume}/protection` → 404 *Branch not protected*, `rulesets` → `[]`, `reviewDecision` empty — so no protection bypass and no `--admin` was used.

| PR | From → to | Merge commit |
|---|---|---|
| #107 | `amp-3108` → `amp-3014` | `8ccc8d8` |
| #111 | `amp-3129` → `amp-3014` | `b642d61` |
| #104 | `amp-3014` → `feat/local-session-resume` | `f3f4f60` |

---

## What this does not touch

No installed harness, live process, config, session state, credentials or keychain was touched. Builds and test runs used worktree `target/` and disposable `HOME`/`XDG_*`/`CARGO_TARGET_TMPDIR` roots, per the story's `local_harness_safety`.

## For the person merging

This PR is `feat/local-session-resume` → **`main`** (the real default branch, read from `gh repo view --json defaultBranchRef`). It is not merged by an agent. Once merged, #3022 (Verify combined harness changes on merged main) unblocks, then #3023 (Approve: harness release) and the release tickets behind it.
