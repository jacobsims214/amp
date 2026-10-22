---
name: github-pr-review
description: GitHub PR review workflow using gh CLI, jq, delta, git — fetching PR diffs, checking CI, line-level review comments, approving or requesting changes. Load when dispatched to review a PR.
---

# GitHub PR Review Skill

Use `gh` CLI for all PR interactions. Pair with `delta` for diff viewing, `jq` for JSON extraction.

## Fetching PR Data

- `gh pr view N --json title,body,state,files,additions,deletions,reviewDecision,mergeable,autoMergeRequest` — full PR info
- `gh pr diff N` — full diff
- `gh pr diff N | delta --line-numbers --dark` — diff with line numbers
- `gh pr commits N --json message,oid,author` — commits in the PR
- `gh pr checks N` — CI status
- `gh pr comments N --json body,author,databaseId` — existing comments

## Posting Reviews

- Comment only: `gh pr review N --comment --body "Overall comment"`
- Approve: `gh pr review N --approve --body "LGTM, minor nits above"`
- Request changes: `gh pr review N --request-changes --body "Needs fixes before merge"`
- Line-level comment: `gh pr review N --comment --body "Suggestion" --commit-id SHA --line 42 --side RIGHT`
- Multiple line comments: pass multiple `--comment` flags (one per comment)

## Review States

| Flag | Effect |
|---|---|
| `--comment` | General comment, no review decision |
| `--approve` | Approve the PR |
| `--request-changes` | Request changes before merge |

## Checking Status

- `gh pr view N --json mergeable` — is it mergeable?
- `gh pr view N --json reviewDecision` — current review state (APPROVED/CHANGES_REQUESTED/REVIEW_REQUIRED)
- `gh pr checks N` — CI check statuses
- `gh pr view N --json autoMergeRequest` — auto-merge status

## Advanced: gh api

- `gh api repos/owner/repo/pulls/N/reviews` — all review data
- `gh api repos/owner/repo/pulls/N/requested_reviewers` — reviewer list
- `gh api repos/owner/repo/pulls/N/comments` — review thread comments

## Limitations

- Line-level comments need the exact commit SHA — get it from `gh pr commits N`
- `--side RIGHT` is for new code, `--side LEFT` for old code
- Cannot resolve review threads — use web UI for that
