---
title: AMP Harness — git and the harness (what needs git, what works without it, worker isolation)
tags: [harness, amp-harness, git, worktrees, isolation, non-git, business-work, workers]
---

# Git and the harness

**Short answer: you don't need git.** AMP Harness works in any folder. Git is how the harness
keeps parallel workers apart when the work is code, and it's on by default in a git repository.
In a folder that isn't one (business or ops work: research, a comparison doc, a customer email,
a spreadsheet), each worker gets a folder of its own instead, and nobody is told to commit or
open a pull request.

## Is this a skill thing or built into the harness?

Both, and it helps to know which part is which.

- **Built into the harness (opinionated):** where a worker runs. When the manager runs a ticket,
  the harness decides whether the worker gets its own git worktree, its own folder, or your
  folder. It decides this from your `[workers] isolation` setting and from what git offers the
  folder. It also tells the worker which one it has. Run reporting fills `branch`, `commit` and
  `pushed` on the ticket only for a worker in a git worktree.
- **Skills (guidance):** what a worker does at the end. `amp-execution` says to commit, push and
  report the branch *when the work is in git*. When the harness says there is no git (`git: none`)
  or no branch of the worker's own, the skill says to skip all of that and to list the files the
  worker made. `git-workflow` (commit and PR conventions) is only for work in git.

So nothing falls over without git. The harness changes what it does, and the skills follow
what the harness tells the worker.

## What needs git, what doesn't

| | needs git? |
|---|---|
| chatting with the manager, planning, the AMP board, approvals, the KB | no |
| workers on tickets, several at once | no: outside git each gets its own folder |
| run records (model, tokens, cost) | no |
| run records' `branch`, `commit`, `pushed` | yes: only workers in a git worktree have them |
| a worker on its own branch, merged later | yes: a git repository with at least one commit |
| reviewers' `git diff` | yes; outside git they read the files the tickets list |
| `amp-github-*` agents, PR review | yes (and GitHub) |

## Where workers work: `[workers] isolation`

Workers that edit files get a place of their own, so two workers running at the same time never
write over each other. Read-only workers (researcher, reviewers) always work in your folder.
(The manager can still run one worker in your folder on purpose with `amp_run_ticket`'s
`worktree: false`; it doesn't wait for other workers.)

| `isolation` | in a git repository with a commit | anywhere else |
|---|---|---|
| `auto` (default) | its own worktree `.worktrees/<ticket>` on branch `amp-<ticket>` | its own folder `amp-work/<ticket>/` |
| `worktree` | its own worktree | refused before the ticket is dispatched, with how to fix it |
| `folder` | its own folder `amp-work/<ticket>/` | its own folder |
| `shared` | your folder; workers that edit files take turns | your folder; they take turns |

"Anywhere else" means a folder that isn't in a git repository, a new folder inside another
repository that doesn't track it, or a repository with no commit yet.

Set it in your config file (it's yours, not the repo's):

```toml
# ~/.config/amp-harness/config.toml
[workers]
isolation = "auto"        # auto | worktree | folder | shared (AMP_HARNESS_WORKER_ISOLATION)
```

or for one run: `AMP_HARNESS_WORKER_ISOLATION=folder amp-harness`. The chat reads it when it
starts. `/config` shows the value in effect and where it came from.

Which one to choose:

- **Code in git:** keep `auto`. Every worker has its own branch, and you review and merge.
  `worktree` does the same, but refuses to run a worker when the folder can't have worktrees,
  so a worker never ends up outside git by accident.
- **Business or ops work in a plain folder:** keep `auto`. Each worker writes its deliverables to
  `amp-work/<ticket>/`. It can read everything in your folder, but it never changes your own
  files, so several workers can run at once. Its completion comment lists the files it made.
- **Work that has to edit your files in place** (update the existing sheet, fix the doc that's
  already there): `shared`. Workers that edit files then run one at a time in your folder.
  Read-only workers still run alongside them.
- **A git repository where you want deliverables, not branches:** `folder`.

## What workers are told

The harness puts it in each worker's context and prompt:

- **Worktree:** `workspace: worktree`, the path, `branch: amp-<ticket>`, `git: yes`, and "commit
  your work there". The worker commits on its branch. The harness records branch, commit and
  pushed on its run.
- **Folder:** `workspace: folder <path>` and `git: none` (or `git: not yours here` inside a
  repository). It is told to put what it makes in its folder, not to change files outside it, not
  to commit, branch, push or open a pull request, and to list its files in the completion
  comment.
- **Shared:** `workspace: shared <path>`, with the same "don't commit" line. It is also told that
  editing workers take turns.

The manager's context gets a `workers_workspace:` line. Outside git worktrees, it tells the
manager not to plan commit, branch, pull-request or merge steps, or review-and-merge tickets. A
person reviewing the deliverable is the gate instead.

## How you'll know

- **Chat banner:** in a folder without git worktrees, one line, for example `not a git
  repository: workers each get a folder amp-work/<ticket>/, no branches or commits`. In a usable
  repository there is no extra line.
- **`amp-harness doctor`:** a `workspace` check, for example `ok workspace isolation auto: /path is
  not in a git repository: each worker that edits files gets its own folder amp-work/<ticket>/
  and nothing is committed; run git init here and commit once to give them git branches`. It is
  never a failure. It warns only when `isolation = "worktree"` can't work in this folder.
- **Setup:** when it links a folder without a usable repository, it says the same.
- **Each worker's report** (`amp_run_ticket`'s result) has `workspace: worktree | folder | shared`
  and its working directory. When `auto` fell back to a folder, a note says why.

## Layout and cleanup

```text
your-repo/                      a git repository
  .worktrees/<ticket>/          a worker's checkout, on branch amp-<ticket>
                                (.worktrees/ is added to .git/info/exclude for you)
your-folder/                    not a git repository
  amp-work/<ticket>/            a worker's deliverables
```

A ticket that runs again reuses its worktree or folder.

- **Worktrees:** once the branch is merged, `git worktree remove .worktrees/<ticket>` and
  `git branch -d amp-<ticket>`. `git worktree prune` tidies entries whose folders are gone.
- **Folders:** move what you want to keep, then delete `amp-work/<ticket>/`. The harness never
  deletes either one for you.

## Switching modes

- **Plain folder to git:** run `git init`, add your files, and commit once (`git add -A && git
  commit -m init`). Restart the chat. Workers get worktrees from the next ticket on.
  `amp-work/` stays where it is: move what you need, or add it to `.gitignore`.
- **Git to folders:** set `isolation = "folder"` and restart the chat.
- `amp-harness doctor` shows what applies after the change.

Related: `harness/running-work.md` (how workers run), `harness/configuration.md` (where settings
live), `harness/install.md` (first run).
