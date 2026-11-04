---
name: code-reviewer
description: Tiered code review framework with Blocker, Significant, Suggestion, and Skip tiers. Use when reviewing Go, TypeScript/React, Dockerfile, or Terraform code changes.
userInvokable: true
---

# Code Reviewer

## Tiers

| Tier | Definition | Action |
|------|-----------|--------|
| Blocker | Breaks correctness, security, or build | Must fix before merge |
| Significant | Wrong pattern, maintenance debt, will cause future bugs | Should fix |
| Suggestion | Style, minor improvement, optional | Take or leave |
| Skip | Trivial, no value in commenting | Don't post |

## Go Review Checklist

| Check | Blocker if | Significant if |
|-------|-----------|----------------|
| Error handling | Errors silently dropped (`_`) | `%s` used instead of `%w` (breaks unwrap chain) |
| Context | Stored in struct field | Missing from I/O function signature |
| Goroutines | Fire-and-forget present | WaitGroup without errgroup for fan-out |
| Panics | In library or handler code | — |
| Interfaces | Importing package only for concrete type | Over-specified (>3 methods unused) |

**Key patterns to enforce:**
- `fmt.Errorf("context: %w", err)` — not `%s`, not string concat
- `errors.Is` / `errors.As` for sentinel checks — not string comparison
- `ctx context.Context` first param on every I/O function
- `*pgxpool.Pool` injected — not created in handlers
- `pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[T])` — not manual `.Scan()` loops
- `errgroup.WithContext(ctx)` for parallel work — not bare `go func()`

## TypeScript/React Checklist

| Check | Blocker if | Significant if |
|-------|-----------|----------------|
| `any` type | In data paths or API response types | Anywhere in codebase |
| Hook rules | Hooks called inside conditions or loops | — |
| Async states | Loading/error not rendered (blank UI) | Only one of the two missing |
| State management | `useEffect` + `useState` for server data | Derived state stored in Zustand |
| Zustand selector | — | `useStore()` whole-store subscription |
| Query invalidation | Mutation with no `invalidateQueries` on success | — |

## Dockerfile Checklist

| Check | Blocker if | Significant if |
|-------|-----------|----------------|
| Root user | Running as root in final stage | No `USER` instruction present |
| Secrets | In `ENV` or `ARG` in image | — |
| Layer order | — | Source copied before deps (busts cache on every code change) |

**Correct layer order:**
```dockerfile
COPY go.mod go.sum ./
RUN go mod download
COPY . .          # source last
```

## Terraform Checklist

| Check | Blocker if | Significant if |
|-------|-----------|----------------|
| Secrets | Hardcoded in `.tf` files | In `tfvars` committed to git |
| Provider versions | Completely unpinned | Uses open range `>= x.y` |
| Required tags | — | Tagged resources missing required tags |
| Module source | Floating branch reference | No version pin |

## Review Format

1. Start with overall verdict (Approve / Approve with fixes / Request changes)
2. Group findings by tier
3. Each finding: `file:line — problem — suggested fix`
4. Be specific: name the exact fix, not just the problem

```
## Overall Verdict
REQUEST CHANGES — one goroutine leak and two missing error wraps.

## Blockers
handler/items.go:42 — goroutine started with no WaitGroup or errgroup; if the handler returns early the goroutine is leaked — use errgroup.WithContext(ctx) and g.Go(...)

## Significant
repo/items.go:18 — fmt.Errorf("query items: %s", err) breaks the error unwrap chain — change %s to %w
repo/items.go:34 — same issue on the CollectRows call

## Suggestions
handler/items.go:60 — consider extracting the pagination logic into a helper for reuse
```

**Rules:**
- Suggest the fix, not just the problem
- Include file path and line number for every finding
- Omit tier sections with zero findings
- Lead with overall verdict so the author knows immediately whether to block
