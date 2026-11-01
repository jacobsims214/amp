---
title: Merging and CI — closing the loop from ticket to main
tags: [merge, ci, pull-request, unmerged, github, webhook, merge-needed]
---

# Merging and CI

A completed ticket isn't shipped until its work is merged. AMP tracks the gap, and the work that
builds on a ticket waits for its merge. Nobody plans a "review and merge" ticket for it.

## Merge needed

When a worker completes a ticket with a pull request or a pushed branch in its run, the ticket is
**Done · Merge needed**.

- **The tickets that depend on it wait for the merge.** They show "Waiting on merge of #N (PR …)"
  and become ready once it's recorded.
- **It waits on the ticket's owner.** It's a **Merge** item in their My work tab
  (`working/human-work.md`), with the PR, how many tickets wait on it, **Review PR ↗** and
  **Mark merged**.
- **A PR closed without merging** shows "PR closed without merging", and the work after it keeps
  waiting. Choose **Reopen ticket** to have the work done again, or **Not needed** to release
  what waits on it.
- **Not needed** is also there for a merge that will never happen (the work was folded into
  another PR, say). It releases the dependents.

Workers make this work by recording their run: branch, commit, PR URL and whether it was pushed
(`amp_complete_task`), and the ticket ref in their commit messages. That's what lets a merge find
its ticket.

**Settings → Merges** turns this off for a project ("Hold dependent tickets until the work they
build on is merged"). Off, dependents start as soon as a ticket is done, merged or not. Agents set
it with `amp_update_project_settings(require_merge)`.

## Integration branches and reviews of open PRs

Two plan shapes need the work after a ticket to start before its merge to main. Each one is a
metadata key, inherited from the project to the epic, story and ticket like any metadata. Setting
one is not a change to the plan, so nothing needs approving again, and it takes effect at once.

- **An integration branch.** An epic's or story's tickets merge into a shared branch (say
  `harness-m5`), and one PR takes that branch to main at the end. Set
  `integration_branch: "harness-m5"` on the epic or story. A worker reports that branch, either
  `branch=harness-m5` when it merged or pushed straight into it, or `base_branch=harness-m5` with
  its `pr_url` when it opened a PR into it. Its ticket is then done without holding anything: the
  next ticket starts on completion. The ticket still counts as unmerged (it's in
  `amp_list_unmerged`, merge state none) until the integration branch reaches main. Then one
  `amp_report_merged(branch="harness-m5")`, or the webhook for the integration PR, marks the whole
  chain merged. The ticket that opens the integration PR to main gates as usual, so whatever comes
  after the integration waits for it. A PR the GitHub webhook sees merged into a ticket's
  integration branch releases that ticket the same way. Setting the key after the fact releases
  tickets whose work already landed there.
- **A review of an open PR.** A ticket that needs the PR open, not merged (a code review), gets
  `waits_for_merge: false`. It starts once the ticket before it is done. Other tickets that depend
  on the same ticket still wait for the merge.

## How a merge is recorded

Once, by whichever comes first. Later reports change nothing and say who recorded it ("already
merged, recorded by GitHub at 14:20").

1. **GitHub**, through the project's merge webhook (below). Merges made on GitHub need no click.
2. **CI** calling `amp_report_merged` (below).
3. **Mark merged**, by a person in My work or the ticket panel. Your agent can record it too, with
   `amp_report_run(task_id, outcome="completed", merged=true, commit=<merge sha>)`, when you tell
   it you merged.

The ticket then reads "Merged (recorded by GitHub · …)", and what waited on it starts.

`amp_list_unmerged(project_id)` lists the done tickets whose work isn't merged yet, with their
`merge_state` (`needed`, or `abandoned` when the PR was closed).

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
