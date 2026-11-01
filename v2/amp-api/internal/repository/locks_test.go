package repository

// Pool-size regression tests (#2598). They run only when TEST_DATABASE_URL
// is set, like kb_test.go.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// withDSNParam adds a key=value parameter to a URL-form DSN.
func withDSNParam(dsn, kv string) string {
	if strings.Contains(dsn, "?") {
		return dsn + "&" + kv
	}
	return dsn + "?" + kv
}

func lockTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the lock tests against Postgres")
	}
	return dsn
}

// A held named lock must not cost a pool connection. amp-api's startup jobs
// (audit checkpoints, audit retention, KB backfill with its per-KB reindex
// lock, the KB reconciler) each hold one while doing work through the pool;
// when the lock kept a pooled connection, four of them at pool_max_conns=4
// (pgxpool's default on a 4-CPU host) left nothing for the work, nor for
// the cluster heartbeat, and amp-api hung at startup (#2598, found by #2123).
func TestTryNamedLock_DoesNotHoldPoolConn(t *testing.T) {
	dsn := withDSNParam(lockTestDSN(t), "pool_max_conns=4")
	ctx := context.Background()
	r, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(r.Close)
	if got := r.db.Config().MaxConns; got != 4 {
		t.Fatalf("pool_max_conns=4 in DATABASE_URL gave MaxConns=%d", got)
	}

	const jobs = 5 // more lock holders than pool connections
	prefix := fmt.Sprintf("test-2598-%d-", time.Now().UnixNano())
	var releases []func()
	defer func() {
		for _, rel := range releases {
			rel()
		}
	}()
	// Bounded: with locks on pooled connections the fifth waits for one.
	lctx, lcancel := context.WithTimeout(ctx, 10*time.Second)
	defer lcancel()
	for i := range jobs {
		rel, ok, err := r.TryNamedLock(lctx, fmt.Sprint(prefix, i))
		if err != nil || !ok {
			t.Fatalf("lock %d: ok=%v err=%v", i, ok, err)
		}
		releases = append(releases, rel)
	}
	if n := r.db.Stat().AcquiredConns(); n != 0 {
		t.Errorf("%d pool connections acquired while only locks are held, want 0", n)
	}

	// Every lock holder does its work through the pool at once.
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, jobs)
	for i := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var one int
			if err := r.db.QueryRow(wctx, `SELECT 1 FROM pg_sleep(0.2)`).Scan(&one); err != nil {
				errs <- fmt.Errorf("job %d query while holding its lock: %w", i, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// The lock is still a session lock: another session can't take it, and
	// it is free again once released.
	other, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect second repo: %v", err)
	}
	defer other.Close()
	if _, ok, err := other.TryNamedLock(ctx, prefix+"0"); err != nil || ok {
		t.Fatalf("lock held elsewhere: ok=%v err=%v, want refused", ok, err)
	}
	releases[0]()
	releases = releases[1:]
	rel, ok, err := other.TryNamedLock(ctx, prefix+"0")
	if err != nil || !ok {
		t.Fatalf("lock after release: ok=%v err=%v", ok, err)
	}
	rel()
}

// Without pool_max_conns in DATABASE_URL the pool gets at least
// MinDefaultPoolConns, whatever the CPU count; with it, the URL wins.
func TestNew_PoolSizeFloor(t *testing.T) {
	dsn := lockTestDSN(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		dsn  string
		want func(int32) bool
		desc string
	}{
		{"default", dsn, func(n int32) bool { return n >= MinDefaultPoolConns }, fmt.Sprintf(">= %d", MinDefaultPoolConns)},
		{"explicit small", withDSNParam(dsn, "pool_max_conns=4"), func(n int32) bool { return n == 4 }, "== 4"},
		{"explicit large", withDSNParam(dsn, "pool_max_conns=40"), func(n int32) bool { return n == 40 }, "== 40"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := New(ctx, tc.dsn)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer r.Close()
			if got := r.db.Config().MaxConns; !tc.want(got) {
				t.Fatalf("MaxConns=%d, want %s", got, tc.desc)
			}
		})
	}
}

func TestHasPoolMaxConns(t *testing.T) {
	for dsn, want := range map[string]bool{
		"postgres://u:p@h:5432/db?sslmode=disable":                     false,
		"postgres://u:p@h:5432/db?sslmode=disable&pool_max_conns=20":   true,
		"postgres://u:p@h:5432/db?pool_max_conns=4":                    true,
		"host=h user=u dbname=db pool_max_conns=8":                     true,
		"host=h user=u dbname=db pool_min_conns=2":                     false,
		"postgres://u:p@h:5432/db?application_name=pool_max_conns_app": false,
	} {
		if got := hasPoolMaxConns(dsn); got != want {
			t.Errorf("hasPoolMaxConns(%q) = %v, want %v", dsn, got, want)
		}
	}
}
