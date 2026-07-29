# Go Engineer Examples

## Error Handling

```go
// Sentinel error definition
var ErrNotFound = errors.New("not found")

// Custom error type
type ValidationError struct {
    Field   string
    Message string
}

func (e *ValidationError) Error() string {
    return fmt.Sprintf("validation: %s %s", e.Field, e.Message)
}

// Wrap with context at each layer
func (s *UserService) GetUser(ctx context.Context, id string) (*User, error) {
    u, err := s.repo.FindByID(ctx, id)
    if err != nil {
        return nil, fmt.Errorf("get user %s: %w", id, err)
    }
    return u, nil
}

// Check sentinel errors through the wrap chain
func handleGet(id string) {
    _, err := svc.GetUser(ctx, id)
    if errors.Is(err, ErrNotFound) {
        http.Error(w, "not found", http.StatusNotFound)
        return
    }
    if err != nil {
        http.Error(w, "internal error", http.StatusInternalServerError)
        return
    }
}
```

## pgx/v5

### Query + CollectRows

```go
func (r *ItemRepo) ListActive(ctx context.Context) ([]*Item, error) {
    rows, err := r.pool.Query(ctx, `
        SELECT id, name, created_at
        FROM items
        WHERE active = $1
        ORDER BY created_at DESC
    `, true)
    if err != nil {
        return nil, fmt.Errorf("query active items: %w", err)
    }

    items, err := pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[Item])
    if err != nil {
        return nil, fmt.Errorf("collect rows: %w", err)
    }
    return items, nil
}
```

### Transaction with Rollback

```go
func (r *OrderRepo) CreateWithItems(ctx context.Context, order Order, items []Item) error {
    tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
    if err != nil {
        return fmt.Errorf("begin tx: %w", err)
    }
    defer tx.Rollback(ctx) // no-op if Commit succeeds

    _, err = tx.Exec(ctx, `INSERT INTO orders (id, total) VALUES ($1, $2)`,
        order.ID, order.Total)
    if err != nil {
        return fmt.Errorf("insert order: %w", err)
    }

    for _, item := range items {
        _, err = tx.Exec(ctx, `INSERT INTO order_items (order_id, product_id, qty) VALUES ($1, $2, $3)`,
            order.ID, item.ProductID, item.Qty)
        if err != nil {
            return fmt.Errorf("insert order item %s: %w", item.ProductID, err)
        }
    }

    if err := tx.Commit(ctx); err != nil {
        return fmt.Errorf("commit: %w", err)
    }
    return nil
}
```

## Concurrency

### errgroup Parallel Fetch (3 independent calls)

```go
func (s *DashboardService) GetSummary(ctx context.Context, userID string) (*Summary, error) {
    g, ctx := errgroup.WithContext(ctx)

    var orders []*Order
    var invoices []*Invoice
    var notifications []*Notification

    g.Go(func() error {
        var err error
        orders, err = s.orderRepo.ListByUser(ctx, userID)
        return fmt.Errorf("list orders: %w", err)
    })

    g.Go(func() error {
        var err error
        invoices, err = s.invoiceRepo.ListByUser(ctx, userID)
        return fmt.Errorf("list invoices: %w", err)
    })

    g.Go(func() error {
        var err error
        notifications, err = s.notifRepo.ListUnread(ctx, userID)
        return fmt.Errorf("list notifications: %w", err)
    })

    if err := g.Wait(); err != nil {
        return nil, fmt.Errorf("get summary: %w", err)
    }

    return &Summary{
        Orders:        orders,
        Invoices:      invoices,
        Notifications: notifications,
    }, nil
}
```

## Testing

### Table-Driven Test with t.Run

```go
func TestValidateEmail(t *testing.T) {
    tests := []struct {
        name    string
        input   string
        wantErr bool
        errMsg  string
    }{
        {"valid email", "user@example.com", false, ""},
        {"missing @", "userexample.com", true, "invalid email"},
        {"empty string", "", true, "email required"},
        {"whitespace only", "   ", true, "email required"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            err := validateEmail(tt.input)
            if tt.wantErr {
                require.Error(t, err)
                assert.Contains(t, err.Error(), tt.errMsg)
                return
            }
            require.NoError(t, err)
        })
    }
}
```

## chi Router

### Handler with Middleware Groups

```go
func NewRouter(h *Handler, auth Middleware, admin Middleware) http.Handler {
    r := chi.NewRouter()

    // Global middleware
    r.Use(middleware.Logger)
    r.Use(middleware.Recoverer)
    r.Use(middleware.RequestID)

    // Public routes
    r.Get("/health", h.Health)
    r.Post("/auth/login", h.Login)

    // Authenticated routes
    r.Group(func(r chi.Router) {
        r.Use(auth)
        r.Get("/items", h.ListItems)
        r.Post("/items", h.CreateItem)
        r.Get("/items/{id}", h.GetItem)

        // Admin-only within authenticated group
        r.With(admin).Delete("/items/{id}", h.DeleteItem)
    })

    return r
}

func (h *Handler) GetItem(w http.ResponseWriter, r *http.Request) {
    id := chi.URLParam(r, "id")
    item, err := h.store.GetItem(r.Context(), id)
    if errors.Is(err, ErrNotFound) {
        http.Error(w, "not found", http.StatusNotFound)
        return
    }
    if err != nil {
        http.Error(w, "internal server error", http.StatusInternalServerError)
        return
    }
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(item)
}
```
