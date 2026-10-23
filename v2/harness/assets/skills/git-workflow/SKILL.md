---
name: git-workflow
description: Use when making commits, naming branches, or opening pull requests — covers conventional commit format, branch prefixes, PR hygiene, and force-push safety rules.
userInvokable: true
---

# Git Workflow

## Commits

Format: `type(scope): description`

| Type | Use for |
|------|---------|
| `feat` | New feature |
| `fix` | Bug fix |
| `chore` | Maintenance, deps, config |
| `docs` | Documentation only |
| `refactor` | No behavior change |
| `test` | Adding or fixing tests |
| `perf` | Performance improvement |

| Rule | Do | Don't |
|------|-----|-------|
| Mood | Imperative: `add feature` | Past tense: `added feature` |
| Length | Under 72 chars subject line | Long run-on subject lines |
| Body | Explain WHY in the body | Repeat WHAT (code shows that) |
| Scope | `feat(api):`, `fix(ui):` | Omit scope on every commit |

## Branches

| Rule | Do | Don't |
|------|-----|-------|
| Prefix | `feat/`, `fix/`, `chore/`, `docs/` | Bare descriptive names |
| Slug | `feat/add-auth-middleware` | `feat/johns-work`, `feat/WIP` |
| Source | Branch from `main` | Branch from other feature branches |
| Characters | Lowercase, hyphens only | Spaces, uppercase, slashes in slug |

## Pull Requests

- **One concern per PR** — if it needs a long explanation, split it
- Description must include: what changed, why, and how to test
- Link the related issue or ticket in the description
- Keep diff under ~400 lines where possible
- Title follows the same conventional commit format as commits

## Rules

| Do | Don't |
|----|-------|
| `--force-with-lease` if you must force push | `--force` on any branch |
| Squash merge to keep history clean | Merge commits on feature PRs |
| Delete branch after merge | Let stale branches accumulate |
| Rebase feature branch onto `main` before PR | Merge `main` into your feature branch |
| `git commit --amend` for local unpushed fixups | `--no-verify` to skip hooks |
| Fix what the hook rejects | Bypass hooks to work around failures |

## Rebase vs Merge

```bash
# Keep your branch up to date — rebase, don't merge
git fetch origin
git rebase origin/main

# Never rebase shared/public branches
```

| Scenario | Use |
|----------|-----|
| Update feature branch with main | `git rebase origin/main` |
| Merge PR into main | Squash merge (GitHub UI: "Squash and merge") |
| Shared branch diverged | Coordinate with team — never force-rebase |

## Commit Examples

```
feat(auth): add JWT refresh token endpoint
fix(api): return 404 when resource not found instead of 500
chore(deps): bump axios from 1.6.0 to 1.7.2
docs(readme): add local development setup instructions
refactor(db): extract query builder into separate package
test(handler): add table-driven tests for pagination logic
perf(cache): replace in-memory map with LRU eviction
```

Use scope when the change is clearly contained to one area. Omit scope for
cross-cutting changes: `chore: update all docker base images to alpine 3.20`.
