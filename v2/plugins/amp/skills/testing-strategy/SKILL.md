---
name: testing-strategy
description: Use when writing or reviewing tests in Go or React/TypeScript — covers table-driven tests, testify assertions, interface mocking, RTL query priority, MSW v2 handlers, and what not to test.
---

# Testing Strategy

## Go — Table-Driven Tests

```go
func TestAdd(t *testing.T) {
    tests := []struct {
        name    string
        a, b    int
        want    int
        wantErr bool
    }{
        {name: "positive numbers", a: 2, b: 3, want: 5},
        {name: "negative result", a: 1, b: -4, want: -3},
        {name: "overflow returns error", a: math.MaxInt, b: 1, wantErr: true},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got, err := Add(tt.a, tt.b)
            if tt.wantErr {
                require.Error(t, err)
                return
            }
            require.NoError(t, err)
            assert.Equal(t, tt.want, got)
        })
    }
}
```

| Rule | Do | Don't |
|------|-----|-------|
| Subtests | `t.Run(tt.name, func(t *testing.T) {...})` | Flat `TestFoo_case1` functions |
| Names | `"empty input returns error"` | `"test1"`, `"case2"` |
| Filtering | Subtests allow `go test -run TestFoo/empty` | Can't filter flat tests |

## Go — Assertions

| Use | When | Instead of |
|-----|------|------------|
| `require.NoError(t, err)` | Setup / preconditions — stops on failure | `if err != nil { t.Fatal(err) }` |
| `assert.Equal(t, want, got)` | Actual result checks — continues on failure | `require` for everything |
| `assert.Equal(t, want, got, "msg")` | Always add a message | No message (hard to debug) |

## Go — Mocking

| Rule | Do | Don't |
|------|-----|-------|
| Target | Define minimal interface; mock the interface | Mock concrete structs |
| Integration | Use real DB for integration tests | Mock the DB for integration tests |
| Depth | Test real behavior at boundaries | Mock 5 layers deep for 3 lines of logic |

## React — RTL Query Priority

| Priority | Query | Use for |
|----------|-------|---------|
| 1st | `getByRole` | Interactive elements — buttons, inputs, links |
| 2nd | `getByLabelText` | Form inputs with labels |
| 3rd | `getByText` | Static text content |
| Last | `getByTestId` | Last resort only — fragile implementation detail |

| Rule | Do | Don't |
|------|-----|-------|
| Events | `userEvent.click(el)` | `fireEvent.click(el)` |
| Behavior | Test what the user sees | Test internal state or refs |
| Async | `await findByRole(...)` | `getByRole` without waiting for async state |

## React — MSW v2

```ts
import { http, HttpResponse } from 'msw'

// Global handler
const handlers = [
  http.get('/api/items', () => HttpResponse.json([{ id: '1' }])),
]

// Per-test override
server.use(
  http.get('/api/items', () => HttpResponse.json({ error: 'oops' }, { status: 500 }))
)
```

| Rule | Do | Don't |
|------|-----|-------|
| Imports | `import { http, HttpResponse } from 'msw'` | `import { rest } from 'msw'` (v1 API) |
| Per-test | `server.use(...)` inside the test | Mutate global handlers |
| Reset | `afterEach(() => server.resetHandlers())` | Shared handler state between tests |

## What NOT to Test

| Don't test | Why |
|------------|-----|
| Snapshot tests of complex UI | Brittle — breaks on minor DOM changes, no behavioral signal |
| Internal component state | Implementation detail — test visible behavior instead |
| Mocked integrations end-to-end | Not testing real behavior |
| Framework behavior (React rendering) | Already tested by React itself |
| Third-party library correctness | Not your code |
