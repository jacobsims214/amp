# Git Workflow

The git-workflow skill provides guidance on making commits, naming branches, and opening pull requests. It covers conventional commit format, branch prefixes, PR hygiene, and force-push safety rules.

## Purpose

This skill is used whenever you need to follow the team's git workflow standards. It ensures consistent commit messages, clear branch naming, and proper pull request practices that make code review easier and maintain a clean project history.

## When to Use

Use this guidance when:
- Creating a new commit for your changes
- Naming a new feature or fix branch
- Opening a pull request
- Deciding whether to rebase or merge branches
- Updating your branch with changes from main
- Squashing or amending commits before pushing

## Commit Format

Commits follow the conventional commits format: `type(scope): description`

### Commit Types

| Type | Use For |
|------|---------|
| `feat` | New features |
| `fix` | Bug fixes |
| `chore` | Maintenance, dependencies, configuration |
| `docs` | Documentation only changes |
| `refactor` | Code changes with no behavior change |
| `test` | Adding or fixing tests |
| `perf` | Performance improvements |

### Commit Best Practices

Write commit messages in the imperative mood, as if giving a command. Use "add feature" not "added feature". Keep the subject line under 72 characters and avoid long run-on sentences.

The commit body should explain why the change was made, not what the code does (the diff shows that). Use a scope when the change is clearly contained to one area, such as `feat(api):` or `fix(ui):`. Omit the scope for cross-cutting changes that affect multiple areas.

### Examples

```
feat(auth): add JWT refresh token endpoint
fix(api): return 404 when resource not found instead of 500
chore(deps): bump axios from 1.6.0 to 1.7.2
docs(readme): add local development setup instructions
refactor(db): extract query builder into separate package
test(handler): add table-driven tests for pagination logic
perf(cache): replace in-memory map with LRU eviction
```

## Branch Naming

Branches should follow a clear naming convention that indicates their purpose and avoids ambiguity.

### Branch Prefix

Prefix your branch with the commit type followed by a slash:
- `feat/` for feature branches
- `fix/` for bug fixes
- `chore/` for maintenance work
- `docs/` for documentation updates

### Branch Slug

After the prefix, use a descriptive slug in lowercase with hyphens. Examples: `feat/add-auth-middleware`, `fix/login-error-handling`. Avoid bare descriptive names, personal names like `feat/johns-work`, or vague names like `feat/WIP`.

### Branch Source

Always branch from `main`, not from other feature branches. This ensures your branch has the most up-to-date code and reduces merge conflicts.

### Branch Characters

Use only lowercase letters and hyphens. Avoid spaces, uppercase letters, and slashes within the slug portion of the branch name.

## Pull Requests

Pull requests should be focused, clear, and easy to review.

### One Concern Per PR

Each pull request should address a single concern or feature. If your change needs a long explanation, it's likely too large and should be split into multiple PRs.

### PR Description

Your description must include:
- What changed
- Why the change was made
- How to test the changes

Link the related issue or ticket in the description so changes can be traced back to their source.

### PR Size

Keep the diff under approximately 400 lines where possible. Large PRs are harder to review thoroughly and more likely to miss bugs.

### PR Title

Follow the same conventional commit format as your commits. For example: `feat(auth): add JWT refresh token endpoint`.

## Rebase and Merge

### Keeping Your Branch Updated

Keep your feature branch up to date with main by rebasing instead of merging:

```bash
git fetch origin
git rebase origin/main
```

Never merge main into your feature branch; always rebase onto it. This keeps a linear history and avoids unnecessary merge commits.

### Force Pushing

When you rebase, you must force push to update the remote branch. Always use `--force-with-lease` instead of `--force`. The `--force-with-lease` option is safer because it will fail if someone else has pushed changes to the branch, preventing accidental overwrites.

Never force push to shared or public branches where other team members may be working.

### Merging Pull Requests

Use squash merge when merging a PR into main. This creates a single commit that represents all the work in the PR, keeping the main branch history clean and linear. In GitHub, use the "Squash and merge" option.

Never merge commits on feature PRs; always rebase your feature branch first.

### Shared Branches

If a shared branch has diverged, coordinate with your team before making changes. Never force-rebase a shared branch without explicit team coordination, as this can cause problems for other developers.

### Local Fixups

For local unpushed commits that need fixing, use `git commit --amend`. This is the appropriate way to fix commits before they are shared with others. Never use `--no-verify` to skip hooks; instead, fix whatever the hook rejects.

## Summary of Rules

| Do | Don't |
|----|-------|
| Use `--force-with-lease` if you must force push | Use `--force` on any branch |
| Squash merge to keep history clean | Merge commits on feature PRs |
| Delete branch after merge | Let stale branches accumulate |
| Rebase feature branch onto main before PR | Merge main into your feature branch |
| Use `git commit --amend` for local unpushed fixups | Use `--no-verify` to skip hooks |
| Fix what the hook rejects | Bypass hooks to work around failures |

## Rebase vs Merge Scenarios

| Scenario | Use |
|----------|-----|
| Update feature branch with main | `git rebase origin/main` |
| Merge PR into main | Squash merge via GitHub UI |
| Shared branch diverged | Coordinate with team — never force-rebase |

By following these guidelines, you help maintain a clean, understandable project history and make the code review process smoother for everyone.