---
name: github-research
description: GitHub research workflow using gh CLI, jq, fzf, delta, bat, git — searching repos, code, issues, PRs, cross-referencing data, checking CI. Load when dispatched for GitHub research.
---

# GitHub Research Skill

Use `gh` CLI for all GitHub interactions. Pair with `jq` for JSON processing, `fzf` for interactive filtering, `delta` for diff viewing, and `bat` for file reading.

## Searching

- `gh search repos "query" --language=go --stars=1000.. --sort=stars` — search repos
- `gh search code "func main" --language=go --repo=owner/repo` — search code in a repo
- `gh search issues "bug" --state=open --label=bug` — search issues
- `gh search pr "fix:" --state=merged --author=user` — search PRs
- Filter flags: `--language`, `--stars`, `--size`, `--created`, `--pushed`, `--state`, `--label`, `--assignee`, `--author`, `--repo`, `--sort`, `--order`

## Viewing

- `gh repo view owner/repo --json description,defaultBranchRef` — repo info
- `gh repo list owner --limit 50` — list repos for org/user
- `gh issue view N` — view issue
- `gh issue list --state open --label bug` — list issues
- `gh pr view N --json title,body,files,additions,deletions` — view PR
- `gh pr diff N` — PR diff
- `gh release list` / `gh release view tag` — releases
- `gh api repos/owner/repo/git/trees/main?recursive=1` — file tree

## CI / Actions

- `gh run list --limit 10` — list workflow runs
- `gh run view ID --log` — view run logs
- `gh run watch ID` — watch run to completion
- `gh workflow list` — list workflows
- `gh workflow view name` — view workflow
- `gh pr checks N` — check CI on a PR

## Cross-referencing

- `gh search code --repo=org/*` — search across all repos in org
- `gh api graphql` with cross-repo GraphQL queries
- `gh api orgs/org/repos` — list all repos in org

## Advanced: gh api

REST: `gh api repos/owner/repo/pulls/N/reviews`
GraphQL: `gh api graphql --field owner=owner --field repo=repo 'query(...) { ... }'`

## Limitations

- No file tree viewer built in — use `gh api .../git/trees` or clone + `tree`
- No commit history viewer — use `git log` directly
- Pagination is manual — use `--paginate` or GraphQL cursors
- Rate limits apply — same as GitHub API