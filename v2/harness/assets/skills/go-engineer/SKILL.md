---
name: go-engineer
description: Go 1.22+ patterns for error handling, pgx/v5, HTTP handlers, chi routing, concurrency, and testing. Use when writing or reviewing Go backend code.
userInvokable: true
---

# Go Engineer

**References:** [Examples](examples.md)

## Error Handling

> [Examples](examples.md#error-handling)

| Rule | Do | Don't |
|------|-----|-------|
| Wrap with context | `fmt.Errorf("get user: %w", err)` | `fmt.Errorf("get user: " + err.Error())` |
| Check errors | `errors.Is(err, ErrNotFound)` | `err.Error() == "not found"` |
| Handle once | return OR log, not both | `log.Error(err); return err` |
| Sentinel errors | `var ErrFoo = errors.New("foo")` | `errors.New("foo")` inline at call site |
| Custom error types | `type FooError struct{ ... }` | `type ErrFooType struct{ ... }` |

```go
var ErrNotFound = errors.New("not found")

func getUser(ctx context.Context, id string) (*User, error) {
    u, err := db.QueryUser(ctx, id)
    if err != nil {
        return nil, fmt.Errorf("get user %s: %w", id, err)
    }
    return u, nil
}
```

## pgx/v5

> [Examples](examples.md#pgxv5)

| Rule | Do | Don't |
|------|-----|-------|
| Positional params | `$1, $2` | `@named` (not standard pgx) |
| Scan rows | `pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[T])` | Manual `.Scan()` loops |
| Pool | Inject `*pgxpool.Pool` | Create new connections in handlers |
| Transactions | `pool.BeginTx(ctx, pgx.TxOptions{})` | `pool.Begin()` |
| Close rows | `defer rows.Close()` | Forget to close |

```go
rows, err := pool.Query(ctx, `SELECT id, name FROM items WHERE active = $1`, true)
if err != nil {
    return nil, fmt.Errorf("query items: %w", err)
}
items, err := pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[Item])
```

## HTTP Handlers

> [Examples](examples.md#http-handlers)

| Rule | Do | Don't |
|------|-----|-------|
| Context first | `func(ctx context.Context, ...)` | Store context in struct field |
| Content-Type | Set before `WriteHeader` | Set after write (ignored) |
| Status | `w.WriteHeader()` before `w.Write()` | Set status after body |
| Encode | `json.NewEncoder(w).Encode(v)` | `json.Marshal` + `w.Write` |
| Early return | `if err != nil { http.Error(...); return }` | Continue after error |

```go
func (h *Handler) GetItem(w http.ResponseWriter, r *http.Request) {
    id := chi.URLParam(r, "id")
    item, err := h.store.GetItem(r.Context(), id)
    if err != nil {
        http.Error(w, "not found", http.StatusNotFound)
        return
    }
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(item)
}
```

## chi Router

> [Examples](examples.md#chi-router)

```go
r := chi.NewRouter()
r.Use(middleware.Logger)
r.Use(middleware.Recoverer)

r.Group(func(r chi.Router) {
    r.Use(authMiddleware)
    r.Get("/items", h.ListItems)
    r.With(adminOnly).Delete("/items/{id}", h.DeleteItem)
})
```

## Concurrency

> [Examples](examples.md#concurrency)

| Rule | Do | Don't |
|------|-----|-------|
| Parallel calls | `errgroup.WithContext(ctx)` | Sequential when independent |
| Bounded | `g.SetLimit(n)` | Unbounded goroutines |
| Always wait | `g.Wait()` | Fire-and-forget |
| Loop var capture | `item := item` before goroutine | Close over loop variable |

```go
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(10)

for _, item := range items {
    item := item
    g.Go(func() error { return process(ctx, item) })
}
return g.Wait()
```

## Testing

> [Examples](examples.md#testing)

| Rule | Do | Don't |
|------|-----|-------|
| Table-driven | `[]struct{ name, input, want }` + `t.Run` | Flat test functions |
| Fatal | `require.NoError(t, err)` | `if err != nil { t.Fatal(err) }` |
| Non-fatal | `assert.Equal(t, want, got)` | `require` for all assertions |
| Mock interfaces | Minimal interface, mock it | Mock concrete types |

```go
func TestGetUser(t *testing.T) {
    tests := []struct {
        name    string
        id      string
        wantErr bool
    }{
        {"valid id", "abc", false},
        {"empty id", "", true},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            _, err := getUser(context.Background(), tt.id)
            if tt.wantErr {
                require.Error(t, err)
                return
            }
            require.NoError(t, err)
        })
    }
}
```
