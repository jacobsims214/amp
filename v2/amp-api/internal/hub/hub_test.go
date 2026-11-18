package hub

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

const testKeepalive = 20 * time.Millisecond

// streamRecorder is a concurrency-safe http.ResponseWriter + http.Flusher
// that records everything the handler writes.
type streamRecorder struct {
	header http.Header

	mu     sync.Mutex
	buf    bytes.Buffer
	writes int
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: make(http.Header)}
}

func (s *streamRecorder) Header() http.Header { return s.header }
func (s *streamRecorder) WriteHeader(int)     {}
func (s *streamRecorder) Flush()              {}

func (s *streamRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	return s.buf.Write(p)
}

func (s *streamRecorder) snapshot() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String(), s.writes
}

// waitFor polls cond until it is true, failing the test after a bound.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func clientCount(h *Hub) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// serve runs ServeFiltered on its own goroutine against a recorder and
// returns the recorder, a cancel that disconnects the client, and a channel
// closed when the handler returns.
func serve(t *testing.T, h *Hub, allow Allow) (*streamRecorder, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeFiltered(rec, req, allow)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return rec, cancel, done
}

func TestServeFiltered_IdleStreamGetsKeepalive(t *testing.T) {
	h := New()
	h.keepalive = testKeepalive
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	r := bufio.NewReader(resp.Body)
	readFrame := func() string {
		t.Helper()
		var lines []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("reading stream: %v (so far %q)", err, lines)
			}
			line = strings.TrimSuffix(line, "\n")
			if line == "" {
				return strings.Join(lines, "\n")
			}
			lines = append(lines, line)
		}
	}

	if got, want := readFrame(), `data: {"type":"connected"}`; got != want {
		t.Fatalf("first frame = %q, want %q", got, want)
	}
	// Nothing is published, so every following frame must be a keepalive.
	for i := 0; i < 3; i++ {
		if got := readFrame(); got != ": keepalive" {
			t.Fatalf("frame %d = %q, want %q", i+1, got, ": keepalive")
		}
		if i == 0 {
			if elapsed := time.Since(start); elapsed < testKeepalive {
				t.Fatalf("first keepalive after %v, want no sooner than %v", elapsed, testKeepalive)
			}
		}
	}

	// A real client disconnect ends the handler and deregisters the stream.
	cancel()
	waitFor(t, "client to be deregistered after disconnect", func() bool { return clientCount(h) == 0 })
}

func TestServeFiltered_DisconnectStopsKeepalive(t *testing.T) {
	h := New()
	h.keepalive = testKeepalive
	rec, cancel, done := serve(t, h, nil)

	waitFor(t, "first keepalive", func() bool {
		out, _ := rec.snapshot()
		return strings.Contains(out, keepaliveFrame)
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeFiltered did not return after the request context ended")
	}
	if n := clientCount(h); n != 0 {
		t.Fatalf("clients after disconnect = %d, want 0", n)
	}

	// The ticker is stopped with the handler: nothing more is written.
	_, before := rec.snapshot()
	time.Sleep(5 * testKeepalive)
	if _, after := rec.snapshot(); after != before {
		t.Fatalf("writes after disconnect: %d, then %d", before, after)
	}
}

func TestServeFiltered_DeliversEventsAlongsideKeepalive(t *testing.T) {
	h := New()
	h.keepalive = 10 * time.Millisecond
	allow := func(projectID int) bool { return projectID == 1 }
	rec, _, _ := serve(t, h, allow)

	waitFor(t, "client to register", func() bool { return clientCount(h) == 1 })
	waitFor(t, "a keepalive", func() bool {
		out, _ := rec.snapshot()
		return strings.Contains(out, keepaliveFrame)
	})

	h.Publish(domain.Event{Type: domain.EventType("test.hidden"), ProjectID: 2})
	h.Publish(domain.Event{Type: domain.EventType("test.visible"), ProjectID: 1})

	waitFor(t, "the allowed event", func() bool {
		out, _ := rec.snapshot()
		return strings.Contains(out, `"type":"test.visible"`)
	})
	if out, _ := rec.snapshot(); strings.Contains(out, "test.hidden") {
		t.Fatalf("filtered event was delivered: %q", out)
	}
}

func TestKeepaliveInterval(t *testing.T) {
	tests := []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{name: "configured", set: 5 * time.Millisecond, want: 5 * time.Millisecond},
		{name: "zero falls back to default", set: 0, want: DefaultKeepalive},
		{name: "negative falls back to default", set: -time.Second, want: DefaultKeepalive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Hub{keepalive: tt.set}
			if got := h.keepaliveInterval(); got != tt.want {
				t.Fatalf("keepaliveInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNew_UsesDefaultKeepalive(t *testing.T) {
	if got := New().keepaliveInterval(); got != 25*time.Second {
		t.Fatalf("default keepalive = %v, want 25s", got)
	}
}
