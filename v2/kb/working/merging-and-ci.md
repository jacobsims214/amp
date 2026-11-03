---
title: Merging and CI — closing the loop from ticket to main
tags: [merge, ci, pull-request, unmerged, github, webhook, merge-needed, integration-branch, provenance]
---

# Merging and CI

A completed ticket isn't shipped until its work is merged. AMP tracks the gap, and the work that
builds on a ticket waits for its merge. Nobody plans a "review and merge" ticket for it.

One PR is one thing to do. A merge is a fact about the code that gets **recorded**, once, from
whatever saw it happen; it is not a second ticket to acknowledge. So a merge shows up in exactly
one place — a **Merge** item in the person's My work tab — and it is gone the moment the merge is
recorded, by any route.

## Merge needed

When a ticket completes with work that has to be merged, it is **Done · Merge needed**. Work has
to be merged when its run opened a pull request or pushed a branch (`pr_url`, or `branch` with
`pushed: true` — recorded via `amp_complete_task` or `amp_report_run`), the project requires merges
and did when the ticket completed, the work is an agent's (a person's ticket never gates), and the
work didn't land on the ticket's integration branch.

- **The tickets that depend on it wait for the merge.** They show "Waiting on merge of #N (PR …)"
  and become ready once it's recorded.
- **It waits on the ticket's owner.** It's a **Merge** item in their My work tab
  (`working/human-work.md`).

A **run that changed nothing** is not work waiting to be merged. AMP Harness compares a worktree
against the commit the run started from and, when the run made no commits, reports no `branch`,
no `commit` and no `pushed` at all (`v2/harness/crates/amp-core/src/run_report.rs`). The run is
still recorded — its model and token spend are — but there is nothing to merge, so the ticket
does not become Merge needed and no one is asked. A worker that pushed anyway, or reused a
worktree that already had commits, says so itself: what a worker reports is never dropped, only
what the harness would have guessed.

### What one Merge item says, and what it refuses to claim

The item is built from facts AMP holds, and it says plainly which facts it lacks.

- **Where the work is.** `PR #N · head branch → target`, from the latest run that reported a PR or
  a branch. The target is the run's `base_branch`.
- **Whether that target is verified.** AMP keeps **no repository metadata** — no repository slug,
  no default branch, no branch protection, no CODEOWNERS — and has no client for GitHub's API. So
  it cannot compute "this is the repo's default branch" or "a rule here requires your approval",
  and it does not guess `main`. A reported target is labelled *as reported* ("can't confirm this
  is the default branch"); an unreported one shows **target & rules: unknown** with a warning, and
  is shown rather than hidden. Unknown is never read as "internal, nothing to do".
- **Who actually waits.** Only dependents that really wait are counted. A dependent with
  `waits_for_merge: false` started when the work completed and is named separately
  ("#2904 opted out"), never counted; a plan's merge step that performs the merge is not a waiter
  on it at all.
- **Nothing waits** is a statement, not a default. When the merge holds up no one, the item says
  so and **Mark merged** is demoted to a quiet manual record — GitHub or CI records it by itself.
- **A PR closed without merging** shows "PR closed without merging", and the work after it keeps
  waiting. Choose **Reopen ticket** to have the work done again, or **Not needed** to release what
  waits on it.
- **Not needed** is also there for a merge that will never happen (the work was folded into
  another PR, say). It releases the dependents.

**Mark merged is an acknowledgement of a merge you made elsewhere**, never the act of merging: it
says so, and it is only worth clicking after you merged on GitHub. Where it can't do its job it
isn't offered: not on a PR closed without merging (there is nothing to mark), and not while a
linked merge step is still gated (see below) — there, recording the merge would leave the step
open anyway, so the one click the item promises can't do both jobs. The reason is written on the
item instead.

## Two plan shapes that don't wait for main

Two plan shapes need the work after a ticket to start before its merge to main. Each one is a
metadata key, inherited from the project to the epic, story and ticket like any metadata. Setting
one is not a change to the plan, so nothing needs approving again, and it takes effect at once —
including after the fact, on tickets whose work is already up.

### An integration branch

An epic or story whose tickets first land on a shared branch, with one PR taking that branch to
main at the end, sets `integration_branch: "<branch>"`.

- **Reporting the destination is what makes it work.** A worker says `branch=<branch>` when it
  merged or pushed straight into it, or `base_branch=<branch>` with its `pr_url` when it opened a
  PR into it. The server reads exactly that (`domain.Run.LandsOn`): a run whose `base_branch` is the
  integration branch belongs there — so its ticket holds nothing, and the next ticket starts on
  completion. A PR whose head is that branch and whose base is something else, or says nothing, is
  the branch's own PR to main and gates as usual.
- **It stays unmerged until the branch reaches main.** The ticket is listed by
  `amp_list_unmerged` with merge state `none` (it awaits a merge, but holds nothing). One
  `amp_report_merged(branch="<branch>")`, or the webhook seeing the integration PR merge, records
  the whole chain.
- **A correction releases a false gate.** A run reported after completion that turns out to land
  on the ticket's integration branch clears the gate (`Service.releaseIntegrationLanding`), so a
  retry that corrects its destination doesn't leave dependents blocked behind a merge to main that
  will never happen. Setting `integration_branch` after the fact does the same for tickets whose
  work already landed there.

**An integration branch is agent organization, not a person's chore.** Landing on it asks nothing
of anyone. There is no acknowledgement for it, and it never appears as a Merge item.

### A review of an open PR

A ticket that needs the PR open, not merged (a code review), gets `waits_for_merge: false`. It
starts once the ticket before it is done. Other tickets that depend on the same ticket still wait
for the merge. Such a ticket is named on the Merge item as an opt-out rather than counted as held
up, because the merge doesn't hold it.

## Plan steps and real sign-offs

A merge can be a step in the plan — but then it must be **the same act** as recording the merge,
not a second card.

- **A planned merge step is linked with `merge_for`.** A person's ticket whose job is to merge one
  PR carries `merge_for: <the id of the agent ticket that produced the PR>`. My work then folds
  that step into the PR's Merge item ("Tracked by #2304") instead of showing a Do card as well, so
  one PR is one thing to do.
- **Readiness is the backend's own rule.** Mark merged appears on a linked step only when the
  server would close it in the same act: not started by its owner, not blocked on a question, and
  every prerequisite done. Otherwise the open prerequisites are listed on the item and Mark
  merged is absent, not disabled. A merge recorded early is still recorded — it's a fact about
  the code, and nothing may overturn the plan — but the step stays open, so the item doesn't
  offer a click that can only half-work.
- **One click does both jobs.** When the step is ready, Mark merged records the merge *and*
  completes the step, and the answer says which happened (`linked_step`: completed,
  already_done, or stays open with the reason).
- **A merge recorded from elsewhere finishes the step too.** GitHub's webhook and CI's
  `amp_report_merged` complete the linked step as the same act, so merging on GitHub leaves the
  person owing nothing. The step's history names the report that finished it — "its tracked merge
  (#N) was recorded by github" — and never reads as a sign-off the person gave. No run is invented
  for them.
- **A step gated by an open prerequisite stays open, on purpose.** The plan says the merge comes
  after that prerequisite. When the prerequisite later finishes, the step is reconciled and
  finished then, so it never goes on asking for a merge that already happened.
- **Finishing the step records the merge**, the other direction. A linked step marked done with
  the PR link records that PR merged on the ticket that produced it, once. It takes **both** — the
  link and a merge still unrecorded — because `merge_for` says which ticket's merge the step
  tracks, not that every completion of the step is a merge; otherwise linking an approval or a
  sign-off to a ticket would claim a merge nobody made.
- **An unlinked merge-looking step is a warning, not a failure.** `amp_check_plan` reports
  `unlinked_merge_step` (a person's ticket whose name says merge but carries no `merge_for`) and
  `duplicate_merge_step` (two steps reaching the same ticket), both as warnings so the plan stays
  GO. Older unlinked merge tickets keep working exactly as before.

A **real sign-off** is different, and stays a human ticket: a release approval, a spend, a legal
or security sign-off, access agents don't have, a decision, a customer's reply. Those are the
person's own judgement, not a fact about the code, and `working/human-work.md` covers them.

## How a merge is recorded

Once, by whichever comes first. Later reports change nothing and say who recorded it ("already
merged, recorded by GitHub at 14:20"). Every route ends in the same place — the ticket reads
"Merged (recorded by GitHub · …)", and what waited on it starts. **Already merged is never an
alert**: a recorded merge's item leaves My work, so no one is asked to acknowledge a merge that
happened.

1. **GitHub**, through the project's merge webhook (below). Merges made on GitHub need no click.
2. **CI** calling `amp_report_merged` (below).
3. **Mark merged**, by a person in My work or the ticket panel. Your agent can record it too, with
   `amp_report_run(task_id, outcome="completed", merged=true, commit=<merge sha>)`, when you tell
   it you merged. Recording a merge is a fact about the code, so anyone on the project may report
   it, and a repeat is a no-op.

`amp_list_unmerged(project_id)` lists the done tickets whose work isn't merged yet, with their
`merge_state` (`needed`, or `abandoned` when the PR was closed). It deliberately lists nothing for
a run that reported no branch and no PR: there is nothing to merge there.

**Settings → Merges** turns the gate off for a project ("Hold dependent tickets until the work
they build on is merged"). Off, dependents start as soon as a ticket is done, merged or not.
Agents set it with `amp_update_project_settings(require_merge)`.

## The GitHub merge webhook

GitHub tells AMP when a pull request is merged or closed. A merged PR records the merge on the
tickets that produced it and releases the tickets waiting on them; a PR closed without merging
marks them as closed without merging. AMP matches a PR to tickets by its URL, its branch (not from
a fork) or its commits, in this project only.

The setting is **Settings → GitHub merge webhook**, which only project owners see. To set it up:

1. In AMP, open the project's **Settings → GitHub merge webhook** and click **Generate secret**.
   Copy it: it's shown only once, when it's generated or rotated.
2. In the GitHub repository, open **Settings → Webhooks → Add webhook**.
3. **Payload URL**: the URL AMP shows (**Copy URL**). It's `/api/hooks/github/<project id>` on
   your AMP.
4. **Content type**: `application/json`.
5. **Secret**: the secret from step 1.
6. **Which events**: choose "Let me select individual events" and tick only **Pull requests**.
7. Add the webhook. GitHub sends a ping, and it shows under **Recent deliveries** in AMP.

The panel shows recent deliveries and what each did, and counts requests AMP rejected: if GitHub
sent them, its secret doesn't match. **Rotate secret** makes a new one (paste it into GitHub; the
old one stops working at once). **Turn off** removes the secret and the delivery log.

GitHub doesn't retry a failed delivery by itself; use **Redeliver** on GitHub's webhook page.

Your AMP must be reachable from GitHub at `/api/hooks/` without a login. AMP's own Envoy lets that
path through (its only check is the signature); anything your deployment puts in front of AMP (an
ingress, a CDN, a WAF) has to pass it through too.

The webhook matches a merged PR to runs by its URL, its **head** branch or its commits — and a
head branch also reaches the runs whose `base_branch` is it, which is how merging an integration
branch into main marks the whole chain merged.

## From CI

After a merge, CI calls the MCP tool `amp_report_merged` with the branch, PR URL or commit
(`branch`, `pr_url`, `commit`) plus the `merge_commit`. Every ticket whose runs produced that work
is marked merged; the tool returns `merged_task_ids`, and `already_merged_task_ids` for the ones
GitHub or a person had already recorded. It is one stateless JSON-RPC POST to `/mcp`:

```sh
curl -fsS "$AMP_URL/mcp" -H "Authorization: Bearer $AMP_TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2026-07-28' -H 'Mcp-Method: tools/call' -H 'Mcp-Name: amp_report_merged' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"amp_report_merged",
       "arguments":{"branch":"feat/my-branch","merge_commit":"<merge sha>"},
       "_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",
                "io.modelcontextprotocol/clientInfo":{"name":"ci","version":"1"},
                "io.modelcontextprotocol/clientCapabilities":{}}}}'
```

AMP API tokens work only on `/mcp`, so don't call AMP's REST paths from CI with one. Use a token
from an agent account made for CI. Read access to the project is enough.

With the GitHub webhook set up, CI doesn't need to do this for GitHub merges. It's for other git
hosts, or for recording merges from a pipeline.
