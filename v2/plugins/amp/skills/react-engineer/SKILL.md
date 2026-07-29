---
name: react-engineer
description: React 18+TypeScript+Vite patterns for Zustand, TanStack Query, Tailwind, TypeScript, and async UX. Use when writing or reviewing React frontend code.
---

# React Engineer

**References:** [Examples](examples.md)

## Zustand (State Management)

> [Examples](examples.md#zustand)

| Rule | Do | Don't |
|------|-----|-------|
| Selector | `useStore(s => s.field)` | `useStore()` whole store |
| Slices | One store file per feature | Giant monolithic store |
| Derived state | Compute in component | Store computed values in store |
| Actions | `set(state => ...)` in store | Call `setState` from components |

```typescript
const items = useItemStore(s => s.items)       // re-renders on items only
const { items, filter } = useItemStore()        // re-renders on any change ❌
```

## TanStack Query (Server State)

> [Examples](examples.md#tanstack-query)

| Rule | Do | Don't |
|------|-----|-------|
| Query key | `['entity', id]` array | String keys `'entity-123'` |
| Stale time | `staleTime: 30_000` for reference data | Default 0 (refetches every focus) |
| Mutations | `useMutation` + `onSuccess: invalidateQueries` | Manual refetch after write |
| Loading states | Handle `isLoading`, `isError`, `data` | Assume data always present |
| Data fetch | `useQuery` | `useEffect` + `useState` for server data |

```typescript
const { data, isLoading, isError } = useQuery({
    queryKey: ['items', id],
    queryFn: () => api.getItem(id),
    staleTime: 30_000,
})
```

## Tailwind

> [Examples](examples.md#tailwind)

| Rule | Do | Don't |
|------|-----|-------|
| Conditional | `cn('base', condition && 'variant')` | `style={{}}` inline |
| Custom CSS | `@layer components` only | Arbitrary CSS files |
| Responsive | `md:grid-cols-2` | JS breakpoint detection |
| Dark mode | `dark:text-white` | Conditional className strings |

```tsx
<div className={cn('rounded px-3 py-2', isActive && 'ring-2 ring-blue-500')}>
```

## TypeScript

> [Examples](examples.md#typescript)

| Rule | Do | Don't |
|------|-----|-------|
| Errors | `catch (e: unknown)` + narrow | `catch (e: any)` |
| API types | Explicit interfaces for all responses | `any` for API data |
| Hook returns | Explicit return type `(): { data: T; loading: boolean }` | Inferred `any` |
| Assertions | `as T` only after validation | Blind type casting |

```typescript
catch (e: unknown) {
    const msg = e instanceof Error ? e.message : 'Unknown error'
    setError(msg)
}
```

## Component Patterns

> [Examples](examples.md#component-patterns)

| Rule | Do | Don't |
|------|-----|-------|
| Custom hooks | Extract logic to `useFeatureName()` | Logic inline in component body |
| Co-locate | State near consumer | Lift state prematurely |
| Memo | Only after profiling | Wrap everything in `React.memo` |
| Props | Explicit interface | Spread unknown props |

```typescript
// Extract into a custom hook
function useItemData(id: string) {
    return useQuery({ queryKey: ['items', id], queryFn: () => api.getItem(id) })
}
```

## Async UX

**Always render three states:** loading skeleton / error message / content.

Never render empty UI while loading — always show a skeleton or spinner.

> [Examples](examples.md#async-states)

```tsx
if (isLoading) return <Skeleton />
if (isError)   return <ErrorMessage error={error} />
return <ItemList items={data} />
```
