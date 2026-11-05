---
title: Verification receipts — how a finished ticket was checked
tags: [receipts, verification, checks, accept, reject, require, inform, self-checked, held]
---

# Verification receipts

A receipt is the record of how an agent checked its work and whether a person accepted it. It
sits at the top of a completed ticket's drawer: who did it, where the code is, how it was checked,
and its status.

## The setting: Off, Inform, Require

Each project has a **Verification receipts** setting in Project settings. The org default is set
on the Orgs page and is **Off** until an org admin changes it.

- **Off**: nothing is recorded or stored. The drawer still shows a receipt built from the worker's
  "Work complete." comment, labelled "from comment".
- **Inform**: each completion records a receipt with its checks, and the status shows on the
  drawer and on the Board card. Nothing waits on it.
- **Require**: as Inform, and a ticket can't be marked done until its receipt reaches the minimum
  level (v1: **Self-checked**). A completion below it is **held**.

Only project owners and org admins change the setting, in the web UI. Agents never do. A ticket,
story or epic can override it with the metadata keys `receipts` (`off`, `inform`, `require`) and
`receipts_min_level`. An agent may only make the override stricter; lowering it takes a person in
the web UI. The drawer says "Requires receipt" when a ticket's effective setting is Require.

Changing the setting affects only later completions. Held tickets stay held until a person acts.
Turn Require on only after an AMP upgrade has finished rolling out: while older and newer servers
run side by side, an older one completes tickets that Require should hold.

## The status

The status is worked out from the checks each time, never stored:

| Status | When |
|---|---|
| **Accepted** | the latest person's verdict is Accept |
| **Independently verified** | a verifier other than the worker passed it (arrives with the verifier) |
| **Self-checked** | the worker's checks include a pass and no fail; "can't tell" doesn't count against it |
| **Unverified** | anything else, including no checks |

A later Reject overrides an earlier Accept; the drawer then says "Rejected by <name>: <note>".
Reopening a ticket and completing it again makes a new receipt. The older ones are listed as
earlier attempts.

## Held: "Couldn't verify, needs you"

Under Require, a completion below the minimum stays **in progress**, its worker's dispatch ends,
and the ticket says "Couldn't verify, needs you" with the reason, on the ticket and on its Board
card. It is a **Verify** item in "Waiting on you" (My work and its counts) for the ticket's owner,
or for the project's owners when it has none. The tickets that depend on it keep waiting. A ticket
that isn't in progress can't be held: below the minimum, its completion is refused.

A hold lasts only while the ticket waits: moving it back to ready, dispatching it again, deleting
it or completing it another way ends the hold.

- **Accept** completes it. A PR on it still waits on its merge, as usual.
- **Reject** needs a note. The ticket goes back to ready with the note as a comment, for the next
  dispatch.

Accept and Reject are a person's, in the web UI only, for anyone who can work the ticket. Accepting
also keeps the receipt (its evidence never expires), and so does recording the ticket's merge.

## For workers

The worker opens a receipt with `amp_open_receipt(task_id, dispatch_id)` before completing. With
`mode: "off"` it completes as before. Otherwise it passes `receipt_id` and one check per acceptance
criterion to `amp_complete_task`: `{criterion_ref, kind, verdict, command?, exit_code?, note?}`,
where `verdict` is `pass`, `fail` or `cant_tell`. A completion that comes back `held: true` is not
done: the worker stops and says why. Older plugins that send no checks keep working in Off and
Inform projects; under Require their completions are held.

Evidence uploads (screenshots, video) and their playback arrive in the next phase.
