---
title: Verification receipts — how a finished ticket was checked
tags: [receipts, verification, checks, accept, reject, require, inform, self-checked, held, evidence, screenshots, video]
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
the web UI. Off the web UI an item's own value can only rise or be set for the first time: removing
it, or lowering it, is refused even where the item's own effective setting wouldn't change, since a
ticket under it may depend on it. A project created off the web UI can't weaken the org default,
and a project imported off the web UI comes without the bundle's receipts keys (a person sets
them). The drawer says "Requires receipt" when a ticket's effective setting is Require.

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

While it waits, agents can't touch it: off the web UI, completing it (its worker calling
`amp_complete_task` again, even with passing checks, or a closeout), moving it to any other state
(back to ready included), blocking it with a question, returning it or deleting it all get a 409
that points to the ticket. Only a person releases it: they Accept or Reject it in the web UI, and
the next dispatch reworks a rejected one.

The minimum level in v1 is **Self-checked** only; Independently verified arrives with the
verifier, and setting it is refused until then.

A hold lasts only while the ticket waits: a person moving it back in the web UI (to ready, say),
deleting it there or marking it done ends the hold.

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

## Evidence: screenshots, video, logs

A `visual` check counts only with evidence: a file the worker uploaded for **this** receipt, on the
revision it completed. The worker declares its files with
`amp_add_evidence(receipt_id, dispatch_id, tree_sha, commit?, files: [{name, bytes, sha256, duration_ms?, frames?}])`
and gets a one-time upload URL for each: an HTTP `PUT` of the bytes, with no auth header, single
use and valid 15 minutes (`curl -fsS -T screenshot-1.png "<url>"`). It then cites each file's
`key` as a visual check's `evidence_key`. A visual pass is shown as "can't tell" when:

- its evidence is missing, wasn't uploaded, or belongs to another receipt ("evidence missing");
- its video recorded no frames ("no frames");
- it was recorded on another revision than the one completed ("evidence recorded on another
  revision": both `tree_sha`s are known and differ);
- the AMP server has no evidence storage ("evidence storage isn't configured").

Allowed names (the server sets the content type from the name): `video.mp4`, `poster.gif`,
`contact.png`, `screenshot-<n>.png`, `trace.zip`, `har.json`, `receipt.json`, `console.txt`,
`transcript.txt`. Each file is at most 200 MiB. An upload whose size or sha256 differs from what
was declared is refused (422) and nothing is stored; calling `amp_add_evidence` again gives a new
URL. A used URL is gone (404) and an expired one says so (410). A file already stored with the
same sha256 needs no upload; the same name with different bytes on the same revision is refused.

The drawer plays the video (seeking works), shows screenshots, and links each file for download.
Only project members who can see the ticket can open them, and they are served so a browser never
runs them as a page. Evidence is not an artifact: it doesn't appear in Artifacts or the review
queue.

**Retention.** Evidence of a receipt nobody kept is deleted after 90 days, and the drawer then says
"Evidence expired on <date>". A ticket's receipts are kept for good once it merges or a person
accepts it. Deleting a ticket keeps them while it is in the trash; purging it, or resetting the
project, deletes them. Where the files live: `operations/where-amp-keeps-data.md`.

To try it locally (`make dev`): `scripts/seed-sample-receipt.sh <ready ticket id>` sets that
ticket's project to Inform, records a 3 s test clip with ffmpeg, uploads it and completes the
ticket with a visual check citing it.
