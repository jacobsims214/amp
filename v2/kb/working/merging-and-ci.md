---
title: Merging and CI — closing the loop from ticket to main
tags: [merge, ci, pull-request, unmerged, github, webhook, merge-needed, integration-branch, provenance, merge-alert, default-branch]
---

# Merging and CI

A completed ticket isn't shipped until its work is merged. AMP tracks the gap, and the work that
builds on a ticket waits for its merge. Nobody plans a "review and merge" ticket for it.

One PR is one thing to do. A merge is a fact about the code that gets **recorded**, once, from
whatever saw it happen; it is not a second ticket to acknowledge. So a merge shows up in exactly
one place — a **Merge** item in the person's My work tab — and it is gone the moment the merge is
recorded, by any route.

**Whether a merge asks a person and whether the work after it waits are two different questions.**
The gate decides what merges hold up; the alert policy decides who is told about them. They are
computed separately, and one never changes the other (see "Alerts are not the gate" below).

## Merge needed

When a ticket completes with work that has to be merged, it is **Done · Merge needed**. Work has
to be merged when its run opened a pull request or pushed a branch (`pr_url`, or `branch` with
`pushed: true` — recorded via `amp_complete_task` or `amp_report_run`), the project requires merges
and did when the ticket completed, the work is an agent's (a person's ticket never gates), and the
work didn't land on the ticket's integration branch.

- **The tickets that depend on it wait for the merge.** They show "Waiting on merge of #N (PR …)"
  and become ready once it's recorded.
- **It waits on the ticket's owner** — if the alert policy says it's worth asking them (below).
  Either way, the gate holds the dependents until the merge is recorded.

A **run that changed nothing** is not work waiting to be merged. AMP Harness compares a worktree
against the commit the run started from and, when the run made no commits, reports no `branch`,
no `commit` and no `pushed` at all (`v2/harness/crates/amp-core/src/run_report.rs`). The run is
still recorded — its model and token spend are — but there is nothing to merge, so the ticket
does not become Merge needed and no one is asked. A worker that pushed anyway, or reused a
worktree that already had commits, says so itself: what a worker reports is never dropped, only
what the harness would have guessed.

## Which merges ask a person

A Merge item reaches My work (and `amp_list_tasks` → `waiting_on_people`) only when one of these
is true:

| The item says | When | What it offers |
|---|---|---|
| **targets the default branch** — chip `default: main · detected from GitHub` | GitHub itself says the PR's target is the repository's detected default branch. Safe even when nothing else could be read | the ordinary card: **Review PR ↗**, plus **Mark merged** and **Not needed** where they do something (below) |
| **a rule requires your approval** — the rule box names the ruleset and what it demands | the PR targets another branch, and a repository rule requires *your* approval. **Not reachable today**: AMP can't yet tell whose approval a rule wants (below), so a demanding rule currently lands on unknown | **Review and approve PR #N ↗** only. **Mark merged** and **Not needed** are absent: the approval happens on GitHub, and the webhook records the merge afterwards |
| **unknown** — warn chip `default: main · rules: unknown`, or `default branch & rules: unknown` | the evidence is missing, stale, partial or ambiguous | the card as it was built, with the warning: **Review PR ↗**, **Mark merged**, **Not needed**. Shown, never hidden |

Two further outcomes ask nobody, and so never appear as a Merge item at all:

- **Internal** — the PR targets a branch that isn't the default, and complete, fresh evidence
  proves no rule requires anyone. Landing on an integration branch is agent organization: there
  is no acknowledgement, no chore, and no card. Two things can keep a merge out of this arm: a
  rule that *does* demand an approval (with AMP unable to attribute it to a person, such a PR lands
  on **unknown**), and an *unprotected* target branch — see "What stays unknown" for why that is
  the common case.
- **None** — GitHub's own pull read says the PR is already merged, even though AMP hadn't been
  told. The item asks nobody and leaves My work; its ticket still reads Merge needed until the
  merge is recorded, because suppressing an alert is not a merge (see "Alerts are not the gate").

**Unknown is a floor, never a fallback.** Positive knowledge is the only thing that can quiet an
item; absence of knowledge never can. A repository AMP can't read keeps showing its merges with
the warn chip — the worst case is one extra card a person could ignore, because the reverse
failure (silently dropping a real merge) is the one this policy exists to prevent. A target AMP
can't verify is never assumed to be `main`, and an unverified item is never labelled "internal,
nothing to do".

The Merge item also says **where the work is** (`PR #N · head branch → target`) and **who really
waits**: only dependents that actually wait are counted. A dependent with `waits_for_merge: false`
started when the work completed and is named separately ("#2904 opted out"), never counted; a
plan's merge step that performs the merge is not a waiter on it at all. **Nothing waits** is a
statement, not a default: when the merge holds up no one, the item says so and **Mark merged** is
demoted to a quiet manual record — GitHub or CI records it by itself.

**Mark merged is an acknowledgement of a merge you made elsewhere**, never the act of merging: it
says so, and it is only worth clicking after you merged on GitHub. Where it can't do its job it
isn't offered: never on a rule alert (the owed act is the approval), not on a PR closed without
merging (there is nothing to mark), and not while a linked merge step is still gated (see below) —
there, recording the merge would leave the step open anyway, so the one click the item promises
can't do both jobs. The reason is written on the item instead.

**A PR closed without merging** shows "PR closed without merging", and the work after it keeps
waiting. Choose **Reopen ticket** to have the work done again, or **Not needed** to release what
waits on it. **Not needed** is also there for a merge that will never happen (the work was folded
into another PR, say): it releases the dependents.

## Where those facts come from

AMP's server reads them from GitHub itself, over REST, with the project's credential:

- the repository's **default branch** (`GET /repos/{owner}/{repo}` → `.default_branch`);
- the PR's **actual target and requested reviewers** (`GET /repos/{owner}/{repo}/pulls/{n}`);
- the **rules that apply to the target branch**, already evaluated across repository and
  organization rulesets (`GET /repos/{owner}/{repo}/rules/branches/{branch}`);
- whether **classic branch protection** could be read (`.../branches/{branch}/protection`).

AMP's server has no checkout, so `origin/HEAD` — or any other local git state — is not one of its
sources, and it never guesses `main`. The **target must be GitHub's own answer** for the PR. A
run's reported `base_branch` is a hint: it can say where a worker *intended* to land and it is
shown as reported, but it never decides which of the arms above an item is in. A run that reported
a branch and no PR has no PR to read, so its item is unknown.

The reads are cached, because a My work page can't be allowed to depend on GitHub being up:

- a repository's default branch and a branch's rules are evidence for **15 minutes** — rule
  content changes in real time, so an older read is not evidence of anything;
- a PR's own facts (target, requested reviewers, whether it merged) are evidence for **60
  seconds**;
- a refresh that failed is not retried for **60 seconds**, so a repository nothing can read does
  not turn every page load into another round of doomed GitHub calls.

**A My work read never waits on GitHub.** It answers from the cache, at most *starts* one bounded
background refresh (20 seconds end to end; at most 8 run at once in an AMP process, and a refresh
beyond that is deferred rather than queued, so the next read tries again), and reports **unknown**
for whatever the cache doesn't have. A slow, rate-limited or unreachable GitHub can make an item
unknown; it can never make the page slow, and it can never make an item disappear.

Facts are stored **per project and per repository**, and read with that project's own credential:
one project's cache can't answer another project's read, so a token's visibility never leaks
across projects.

### The webhook empties the cache a delivery contradicts

The TTLs are the backstop for when nothing tells AMP that GitHub changed. The project's merge
webhook (below) is that something-changed, and its write to the cache is deliberately narrow:

- **Every `pull_request` delivery drops that pull request's cached facts**, not just `closed`.
  `edited` and `review_requested` change exactly the target and reviewers the policy reads, and
  are the deliveries that most often precede a wrong alert.
- **Only that PR's row, and only in the project in the URL.** A delivery to one project's hook
  cannot touch another project's copy of the same repository — that copy simply stays stale until
  its own TTL, which is the safe direction.
- **The repository and branch rows survive.** A pull-request event says nothing about the default
  branch or a branch's rules, and dropping them would throw away verified evidence. Renaming the
  default branch is a `repository` event, which the merge webhook isn't subscribed to (it takes
  pull requests only), so there the 15-minute TTL is the backstop.
- It also clears the failed-refresh memory for that PR, so the next read refills it at once
  instead of being silenced by the negative cache for up to a minute.
- If the row can't be dropped, that is logged and the delivery's own work — recording a merge —
  still succeeds. The TTL still bounds how long the stale row can be believed.

## Settings → GitHub token

The credential the reads use is the project's, and only project owners manage it
(**Settings → GitHub token**): the panel is rendered for owners, and every call behind it refuses a
member or a team lead with a 403 rather than a quiet success. Agents never touch it — the server
reads with it on their behalf.

- **It is write-only.** The token goes in, is sealed at rest, and never comes back out: the panel
  shows the host, a label and when it was attached or replaced, never the value. No API answer, no
  error message and no audit entry carries it — the audit records *that* it was attached, replaced
  or removed, and for which host.
- **Attach, replace, remove.** **Replace token** swaps it in place on the same credential, so what
  the project already read stays its own view. **Remove token** deletes the credential *and every
  repository fact it produced*, so no token-derived visibility outlives the token.
- **The server needs an encryption key.** Without `AMP_SECRETS_KEY` (`ampApi.secretsEncryption` in
  the Helm chart) AMP refuses to store a token at all — the panel disables the field and says why
  — and a token already stored can't be opened, so the reads stay unknown until the key is back.
- **No token is a real state.** A public repository is read without one. Attaching a token is what
  makes a private repository (or a rate-limited one) readable at all, and it is what decides how
  much of the repository the project can see.
- **What it needs:** read access to the repository. `Metadata: read` covers the default branch and
  the evaluated rules; `Pull requests: read` covers the PR, its target and its reviewers. Classic
  branch protection is the one endpoint that needs admin — a member token gets a 404 there, which
  is why the rules are treated as *partially* known, never as "no rule" (see below).
- The token is sent in the request body only (never a query string, which every proxy logs), and
  the panel keeps no copy of it after saving — no storage, no telemetry.

## What stays unknown, on purpose

These are limits of the shipped mechanism, not bugs to work around. Each one ends at the warn
chip, never at silence:

- **No credential, or a private repository the credential can't see.** GitHub answers 404 for "no
  such repository" and "you can't see it" alike, so neither is read as a fact.
- **A credential GitHub refuses**: revoked, expired, missing a scope, or rate-limited. A rate-limit
  answer with a short advertised wait gets one retry; there is no backoff loop.
- **Timeouts, transport failures, 5xx, an unreadable body** — unknown, per fact.
- **A renamed or transferred repository** (GitHub's redirect) is reported, not followed: following
  it would record the new repository's facts under the old name.
- **Anything not on `github.com`.** A PR URL on another host is refused as unsupported. GitHub
  Enterprise Server needs its own host and credential decision, and GitLab, Bitbucket and the rest
  have different rule models entirely — none of them are implemented.
- **A run with no PR URL** (a branch, or a URL AMP can't parse): the repository and PR number the
  policy needs aren't there, so the item is unknown.
- **Rules that AMP can't yet attribute to a person.** AMP stores no GitHub login for a user and
  resolves no CODEOWNERS ownership, and it does not resolve team membership, so a rule that names
  a reviewer, a team, or code owners, and a rule that requires *someone's* approval (one approving
  review, an approval from someone other than the last pusher, an extra approval for unattributed
  changes) all become **unknown** — never a rule alert addressed to the wrong person, and never
  "internal" either. This is why the rule arm can't be reached today. A requested reviewer on its
  own is never treated as proof that a rule requires them: GitHub auto-requests code owners
  whether or not anything is protected.
- **An unprotected target branch.** Being unprotected is not an error, but GitHub answers the
  classic-protection endpoint with `404 Branch not protected` — the same 404 a token without
  permission gets. AMP cannot tell those apart, so the protection read counts as unanswered and
  the rules are **partial** for any branch without classic protection. Proving "no rule applies"
  needs a *complete* read, so a non-default target without classic branch protection lands on
  **unknown** rather than internal. (Verified live against this repository on 2026-11-04: the
  evaluated rules for `fix/actionable-merge-workflow` return `[]`, and its protection endpoint
  answers `404 Branch not protected`. The reader's own tests pin the same rule — "a 404 is not
  proof the branch is unprotected".) That is the safe direction — the item is shown — but it does
  mean the quiet internal arm needs a target whose protection endpoint actually answers.
- **A partial rules read's fallback never fires.** When the rules are only partially known, AMP
  would fall back to the PR's own signals: a review GitHub says is required, plus this person
  among the requested reviewers. The stored PR facts never include GitHub's review decision or
  mergeable state, so that fallback cannot fire and a partial read stays **unknown**.
- **Classic branch protection's own settings.** AMP reads that endpoint only to decide whether the
  rules read is complete; the review requirements configured there are not turned into rule
  evidence, because AMP's rule evidence is the evaluated rulesets. So a non-default branch
  protected *only* by classic branch protection has no rule for AMP to name.
- **A manual default-branch setting.** The policy has a slot for a person's declaration (one that
  doesn't age out on the TTL), but no settings surface writes it yet, so a repository whose default
  branch can't be read stays unknown even if everyone knows the answer.
- **The cache itself being unreadable** is not a fact about the repository, so it is not "no rule
  applies" either: unknown, and the item keeps asking the person, which is the safe direction.

## Alerts are not the gate

The two are computed separately, and this matters when you're reading a ticket that says **Merge
needed** but isn't in anyone's My work:

- **The gate alone** decides `merge_state` and which tickets wait
  (`Service.mergeGateFor`). Suppressing an alert never records a merge and never releases a
  dependent: an internal merge's ticket still reads Merge needed until the branch's merge is
  recorded, and its dependents still wait.
- **Mission's "Merge needed" counter and `amp_list_unmerged`** are the gate's bookkeeping, not a
  list of chores. Mission counts done tickets whose `merge_state` is `needed` or `abandoned`, and
  `amp_list_unmerged` lists the runs whose work isn't merged — including integration-branch work
  whose `merge_state` is `none`. Neither is filtered by the alert policy, so a merge nobody is
  asked about still shows up there while it is unrecorded.
- **A planned merge step for a PR nobody is asked about** is not folded into a Merge item, because
  there is no item: it stays an ordinary human ticket (a **Do** item). The merge is still somebody's
  ticket; it just isn't a *merge alert*.
- **An alert policy that goes quiet is not a sign-off.** It says "no one needs to be told about
  this merge", not "this merged".

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
- **Reading the integration branch's rules is how the policy knows it's internal.** AMP asks
  GitHub for the rules on that target branch; if the read is complete and nothing requires
  anyone, the item asks nobody. A branch where a rule *does* require a human approval is not
  quiet — it is either a rule alert or, while AMP can't attribute the rule to a person, unknown.

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

`amp_list_unmerged(project_id)` lists the unmerged work: for each done ticket, the run that put it
up, with the ticket's `merge_state` — `needed` (Merge needed), `abandoned` (the PR was closed
without merging), or `none` (work that landed on its integration branch, which holds nothing until
that branch reaches main). It deliberately lists nothing for a run that reported no branch and no
PR: there is nothing to merge there. This list is about what is unmerged, not about who is asked,
so it includes merges the alert policy keeps off My work.

**Settings → Merges** turns the gate off for a project ("Hold dependent tickets until the work
they build on is merged"). Off, dependents start as soon as a ticket is done, merged or not.
Agents set it with `amp_update_project_settings(require_merge)`.

## The GitHub merge webhook

GitHub tells AMP when a pull request is merged or closed. A merged PR records the merge on the
tickets that produced it and releases the tickets waiting on them; a PR closed without merging
marks them as closed without merging. AMP matches a PR to tickets by its URL, its branch (not from
a fork) or its commits, in this project only. The same delivery also drops the cached repository
facts it contradicts (see above).

The setting is **Settings → GitHub merge webhook**, which only project owners see. To set it up:

1. In AMP, open the project's **Settings → GitHub merge webhook** and click **Generate secret**.
   Copy it: it's shown only once, when it's generated or rotated.
2. In the GitHub repository, open **Settings → Webhooks → Add webhook**.
3. **Payload URL**: the URL AMP shows (**Copy URL**). It's `/api/hooks/github/<project id>` on
   your AMP.
4. **Content type**: `application/json`.
5. **Secret**: the secret from step 1.
6. **Which events**: choose "Let me select individual events" and tick only **Pull requests**.
   That is also all the cache invalidation above needs; other events aren't subscribed to and
   change nothing.
7. Add the webhook. GitHub sends a ping, and it shows under **Recent deliveries** in AMP.

The panel shows recent deliveries and what each did — including when a delivery dropped a cached
pull request's facts — and counts requests AMP rejected: if GitHub sent them, its secret doesn't
match. **Rotate secret** makes a new one (paste it into GitHub; the old one stops working at once).
**Turn off** removes the secret and the delivery log.

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
