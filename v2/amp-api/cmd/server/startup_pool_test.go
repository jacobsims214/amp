package main

// amp-api must start and serve on a small database pool (#2598, found by
// the harness smoke CI in #2123). On a 4-CPU host pgxpool's default is 4
// connections. The startup jobs (audit checkpoints, audit retention, KB
// backfill with its per-KB reindex lock, KB reconciler) and KB reindexes
// each took their advisory lock with repository.TryNamedLock, which kept a
// pooled connection for as long as the lock was held, and then asked the
// pool for more to do their work. Four held locks left nothing: the jobs
// waited on each other for good, the cluster heartbeat failed, /ready went
// 503 and every MCP call timed out.
//
// This builds the real binary and runs it with pool_max_conns=4 against
// its own throwaway database (created and dropped through
// TEST_DATABASE_URL), so its replica row and locks never meet the other
// tests'. A stand-in Typesense holds the KB backfill and two org KB
// reindexes inside their locks until the test lets them go: four named
// locks held at once, which used to be the whole pool.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// startupTestDB creates an empty database next to TEST_DATABASE_URL's and
// returns its URL; it is dropped when the test ends.
func startupTestDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set TEST_DATABASE_URL to run the startup test against Postgres")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" {
		t.Skipf("TEST_DATABASE_URL is not a URL (%v); the startup test needs one", err)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	name := fmt.Sprintf("amp_startup_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Skipf("can't create a database for the startup test: %v", err)
	}
	t.Cleanup(func() {
		defer admin.Close(context.Background())
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	u.Path = "/" + name
	return u.String()
}

// stallingTypesense is a stand-in Typesense with one project KB
// (kb_424242) for the backfill to copy. Requests about that KB, or about a
// KB named with stall, wait until release is closed (or Typesense's 10s
// client timeout): the backfill and reindexes that make them hold their
// named locks meanwhile.
type stallingTypesense struct {
	srv      *httptest.Server
	release  chan struct{}
	backfill chan struct{} // closed when the backfill reaches kb_424242
	once     sync.Once
	stopOnce sync.Once
	mu       sync.Mutex
	stalls   []string
}

func (f *stallingTypesense) stall(collection string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stalls = append(f.stalls, collection)
}

func (f *stallingTypesense) stalled(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.stalls {
		if strings.HasSuffix(path, "/"+c) || strings.Contains(path, "/"+c+"/") {
			return true
		}
	}
	return false
}

func newStallingTypesense(t *testing.T) *stallingTypesense {
	f := &stallingTypesense{release: make(chan struct{}), backfill: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case p == "/health":
			fmt.Fprint(w, `{"ok":true}`)
			return
		case p == "/collections":
			fmt.Fprint(w, `[{"name":"kb_424242","num_documents":1,"fields":[]}]`)
			return
		case p == "/aliases":
			fmt.Fprint(w, `{"aliases":[]}`)
			return
		case strings.Contains(p, "kb_424242"):
			f.once.Do(func() { close(f.backfill) })
			f.wait(r)
		case f.stalled(p):
			f.wait(r)
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	t.Cleanup(func() { f.unstall(); f.srv.Close() })
	return f
}

func (f *stallingTypesense) wait(r *http.Request) {
	select {
	case <-f.release:
	case <-r.Context().Done():
	case <-time.After(45 * time.Second):
	}
}

func (f *stallingTypesense) unstall() { f.stopOnce.Do(func() { close(f.release) }) }

// startupMCPCall makes a stateless (2026-07-28) tools/call with a bounded
// wait, and returns the tool's text or an error.
func startupMCPCall(mcpURL, tool string, args map[string]any) (string, error) {
	b, _ := json.Marshal(modernToolCall(1, tool, args))
	req, _ := http.NewRequest(http.MethodPost, mcpURL, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range modernHeaders("tools/call", tool) {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "data: ") {
				text = strings.TrimPrefix(line, "data: ")
			}
		}
	}
	var body struct {
		Result *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(text), &body) != nil || body.Result == nil || len(body.Result.Content) == 0 {
		return "", fmt.Errorf("%s: %d %s", tool, resp.StatusCode, raw)
	}
	if body.Result.IsError {
		return "", fmt.Errorf("%s failed: %s", tool, body.Result.Content[0].Text)
	}
	return body.Result.Content[0].Text, nil
}

func TestStartup_SmallPool(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs amp-api")
	}
	plainDSN := startupTestDB(t)
	dsn := plainDSN
	if strings.Contains(dsn, "?") {
		dsn += "&pool_max_conns=4"
	} else {
		dsn += "?pool_max_conns=4"
	}

	bin := filepath.Join(t.TempDir(), "amp-api")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	ts := newStallingTypesense(t)
	mcpAddr, apiAddr, clusterAddr := freeAddr(t), freeAddr(t), freeAddr(t)
	cmd := exec.Command(bin)
	// A clean environment: no OIDC (auth off, a local-dev global admin), no
	// Redis, no Ollama, no AMP guide sync.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"DATABASE_URL=" + dsn,
		"MCP_ADDR=" + mcpAddr,
		"API_ADDR=" + apiAddr,
		"CLUSTER_ADDR=" + clusterAddr,
		"CLUSTER_ADVERTISE_ADDR=" + clusterAddr,
		"TYPESENSE_URL=" + ts.srv.URL,
		"TYPESENSE_API_KEY=test",
		"OLLAMA_URL=http://127.0.0.1:1",
		"AMP_GLOBAL_KB_SYNC=off",
		"AUDIT_RETENTION_DAYS=30", // the retention job runs too
		"KB_RECONCILE_INTERVAL=1s",
	}
	logs := &syncBuffer{} // relay_test.go
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		ts.unstall()
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	})
	defer func() {
		if t.Failed() {
			t.Logf("amp-api output:\n%s", logs.String())
		}
	}()

	probe := &http.Client{Timeout: 5 * time.Second}
	ready := func() bool {
		resp, err := probe.Get("http://" + apiAddr + "/ready")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode == http.StatusOK
	}
	// readyStreak waits (bounded) for /ready to answer 200 n times in a row,
	// a second apart.
	readyStreak := func(n int, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for streak := 0; streak < n; {
			select {
			case err := <-exited:
				t.Fatalf("amp-api exited: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("/ready not 200 %d times in a row within %s", n, within)
			}
			if ready() {
				streak++
			} else {
				streak = 0
			}
			time.Sleep(time.Second)
		}
	}
	mcpURL := "http://" + mcpAddr + "/"

	// 1. The backfill holds its run lock and kb_424242's reindex lock.
	select {
	case <-ts.backfill:
	case err := <-exited:
		t.Fatalf("amp-api exited: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the KB backfill never reached kb_424242")
	}
	readyStreak(1, 30*time.Second)

	// 2. Two org KB reindexes hold a lock each: four named locks in all.
	for i := range 2 {
		out, err := startupMCPCall(mcpURL, "amp_create_org", map[string]any{"name": fmt.Sprintf("pool org %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		var created struct {
			Org struct {
				ID int `json:"id"`
			} `json:"org"`
		}
		if err := json.Unmarshal([]byte(out), &created); err != nil || created.Org.ID == 0 {
			t.Fatalf("amp_create_org: %v %s", err, out)
		}
		ts.stall(fmt.Sprintf("kb_org_%d", created.Org.ID))
		out, err = startupMCPCall(mcpURL, "amp_kb_reindex", map[string]any{"scope": "org", "org_id": created.Org.ID})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `"started"`) {
			t.Fatalf("amp_kb_reindex: %s, want started", out)
		}
	}

	// 3. With those four locks held, the service still serves: the
	// heartbeat keeps /ready up and MCP calls reach the database.
	time.Sleep(2500 * time.Millisecond) // more than one heartbeat
	for _, tool := range []string{"amp_whoami", "amp_list_projects"} {
		if _, err := startupMCPCall(mcpURL, tool, map[string]any{}); err != nil {
			t.Fatalf("while four named locks are held: %v", err)
		}
	}
	if !ready() {
		t.Fatal("/ready not 200 while four named locks are held")
	}

	// 4. Let the stalled jobs go: they finish, and every lock comes back.
	ts.unstall()
	deadline := time.Now().Add(60 * time.Second)
	for _, s := range []string{"kb backfill run done", "kb reconcile pass done"} {
		for !strings.Contains(logs.String(), s) {
			if time.Now().After(deadline) {
				t.Fatalf("no %q log line: the job never finished", s)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	ctx := context.Background()
	db, err := pgx.Connect(ctx, plainDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close(ctx)
	for {
		// TryNamedLock's two-key locks (classid = namedLockSpace, "amp"),
		// except the reconciler's: it runs every second, so may be caught
		// holding its lock.
		var held int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_database d ON d.oid = l.database
			WHERE l.locktype = 'advisory' AND l.objsubid = 2 AND l.classid = $1 AND d.datname = current_database()
			  AND l.objid::bigint <> (hashtext('kb_reconcile')::bigint & 4294967295)`, 0x616d70).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d named locks still held after the jobs were let go", held)
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, tool := range []string{"amp_whoami", "amp_list_projects"} {
		if _, err := startupMCPCall(mcpURL, tool, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	readyStreak(3, 30*time.Second)
	if strings.Contains(logs.String(), "heartbeat failed") {
		t.Error("amp-api logged a failed heartbeat")
	}
}
