package hub

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// waitFor polls cond until it is true, failing the test after 2 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForWithin(t, 2*time.Second, what, cond)
}

// waitForWithin polls cond until it is true, failing the test after limit.
func waitForWithin(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
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
	rec := newStreamRecorder()
	cancel, done := serveWriter(t, h, rec, allow, nil)
	return rec, cancel, done
}

// serveWriter is serve against any writer. unblock, if set, runs at cleanup
// before the handler is awaited, to free a write the test left blocked.
func serveWriter(t *testing.T, h *Hub, w http.ResponseWriter, allow Allow, unblock func()) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	return serveWriterAs(t, h, w, 0, allow, unblock)
}

// serveWriterAs is serveWriter for userID's stream.
func serveWriterAs(t *testing.T, h *Hub, w http.ResponseWriter, userID int, allow Allow, unblock func()) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeFiltered(w, req, userID, allow)
	}()
	t.Cleanup(func() {
		if unblock != nil {
			unblock()
		}
		cancel()
		<-done
	})
	return cancel, done
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

const testWriteTimeout = 50 * time.Millisecond

// Where a stuck deadlineWriter blocks.
const (
	stuckInWrite = "write"
	stuckInFlush = "flush"
)

// deadlineWriter is a streaming http.ResponseWriter that honours write
// deadlines the way a net/http connection does: SetWriteDeadline arms the
// connection, and the armed deadline covers the write and its flush. Once
// stuck, a write or a flush (per stuckIn) blocks like one to a client that
// stopped reading, then fails with os.ErrDeadlineExceeded when the deadline
// passes. It counts every write that had no fresh deadline before it.
type deadlineWriter struct {
	header  http.Header
	stuckIn string

	release     chan struct{} // closed by unblock, so a write with no deadline can't hang the test
	releaseOnce sync.Once

	mu       sync.Mutex
	deadline time.Time // armed deadline; zero means none
	fresh    bool      // a deadline was set since the last write
	stuck    bool
	frames   []string
	ahead    []time.Duration // per write: how far ahead its deadline was
	unarmed  int             // writes with no fresh deadline before them
	timedOut int             // stuck writes or flushes that failed on their deadline
}

func newDeadlineWriter(stuckIn string) *deadlineWriter {
	return &deadlineWriter{header: make(http.Header), stuckIn: stuckIn, release: make(chan struct{})}
}

func (d *deadlineWriter) Header() http.Header { return d.header }
func (d *deadlineWriter) WriteHeader(int)     {}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadline = t
	d.fresh = !t.IsZero()
	return nil
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	d.mu.Lock()
	if !d.fresh {
		d.unarmed++
	}
	d.fresh = false
	d.frames = append(d.frames, string(p))
	d.ahead = append(d.ahead, time.Until(d.deadline))
	stuck := d.stuck && d.stuckIn == stuckInWrite
	d.mu.Unlock()
	if stuck {
		if err := d.block(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush satisfies the http.Flusher check in ServeFiltered;
// http.ResponseController calls FlushError instead.
func (d *deadlineWriter) Flush() { _ = d.FlushError() }

func (d *deadlineWriter) FlushError() error {
	d.mu.Lock()
	stuck := d.stuck && d.stuckIn == stuckInFlush
	d.mu.Unlock()
	if stuck {
		return d.block()
	}
	return nil
}

// block waits out the armed deadline, as a write to a full socket does.
func (d *deadlineWriter) block() error {
	d.mu.Lock()
	deadline := d.deadline
	d.mu.Unlock()
	if deadline.IsZero() {
		<-d.release
		return errors.New("blocked write had no deadline; released at cleanup")
	}
	select {
	case <-time.After(time.Until(deadline)):
	case <-d.release:
		return errors.New("blocked write released at cleanup")
	}
	d.mu.Lock()
	d.timedOut++
	d.mu.Unlock()
	return os.ErrDeadlineExceeded
}

// stopReading makes the next write or flush block until its deadline.
func (d *deadlineWriter) stopReading() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stuck = true
}

func (d *deadlineWriter) unblock() { d.releaseOnce.Do(func() { close(d.release) }) }

func (d *deadlineWriter) snapshot() (frames []string, ahead []time.Duration, unarmed, timedOut int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.frames...), append([]time.Duration(nil), d.ahead...), d.unarmed, d.timedOut
}

func countFrames(frames []string, cond func(string) bool) int {
	n := 0
	for _, f := range frames {
		if cond(f) {
			n++
		}
	}
	return n
}

func TestServeFiltered_EveryWriteHasAFreshDeadline(t *testing.T) {
	const timeout = 5 * time.Second // not the default, to show the override is used
	h := New()
	h.keepalive = 10 * time.Millisecond
	h.writeTimeout = timeout
	dw := newDeadlineWriter("")
	cancel, done := serveWriter(t, h, dw, nil, dw.unblock)

	waitFor(t, "client to register", func() bool { return clientCount(h) == 1 })
	waitFor(t, "two keepalives", func() bool {
		frames, _, _, _ := dw.snapshot()
		return countFrames(frames, func(f string) bool { return f == keepaliveFrame }) >= 2
	})
	h.Publish(domain.Event{Type: domain.EventType("test.one"), ProjectID: 1})
	h.Publish(domain.Event{Type: domain.EventType("test.two"), ProjectID: 1})
	waitFor(t, "both events", func() bool {
		frames, _, _, _ := dw.snapshot()
		return countFrames(frames, func(f string) bool { return strings.Contains(f, `"type":"test.two"`) }) == 1
	})
	cancel()
	<-done

	frames, ahead, unarmed, _ := dw.snapshot()
	if frames[0] != "data: {\"type\":\"connected\"}\n\n" {
		t.Fatalf("first frame = %q, want the connected frame", frames[0])
	}
	if unarmed != 0 {
		t.Fatalf("%d of %d writes had no fresh write deadline before them: %q", unarmed, len(frames), frames)
	}
	for i, a := range ahead {
		if a <= timeout-time.Second || a > timeout {
			t.Fatalf("write %d (%q) had its deadline %v ahead, want about %v", i, frames[i], a, timeout)
		}
	}
}

func TestServeFiltered_ClientThatStopsReadingEndsTheStream(t *testing.T) {
	tests := []struct {
		name    string
		trigger string // what makes the blocked write: "keepalive" or "event"
		stuckIn string
	}{
		{name: "event write blocks", trigger: "event", stuckIn: stuckInWrite},
		{name: "event flush blocks", trigger: "event", stuckIn: stuckInFlush},
		{name: "keepalive write blocks", trigger: "keepalive", stuckIn: stuckInWrite},
		{name: "keepalive flush blocks", trigger: "keepalive", stuckIn: stuckInFlush},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New()
			h.writeTimeout = testWriteTimeout
			h.keepalive = time.Hour // only the event writes
			if tt.trigger == "keepalive" {
				h.keepalive = 10 * time.Millisecond
			}
			dw := newDeadlineWriter(tt.stuckIn)
			_, done := serveWriter(t, h, dw, nil, dw.unblock)

			waitFor(t, "client to register", func() bool { return clientCount(h) == 1 })
			waitFor(t, "the connected frame", func() bool {
				frames, _, _, _ := dw.snapshot()
				return len(frames) >= 1
			})

			dw.stopReading()
			stoppedAt := time.Now()
			if tt.trigger == "event" {
				h.Publish(domain.Event{Type: domain.EventType("test.stuck"), ProjectID: 1})
			}

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("ServeFiltered still running 2s after the client stopped reading: the write deadline did not end it")
			}
			if elapsed := time.Since(stoppedAt); elapsed < testWriteTimeout {
				t.Fatalf("stream ended %v after the client stopped reading, before the %v deadline could pass", elapsed, testWriteTimeout)
			}
			if n := clientCount(h); n != 0 {
				t.Fatalf("clients after the stuck write = %d, want 0 (subscriber not unregistered)", n)
			}

			frames, _, unarmed, timedOut := dw.snapshot()
			if timedOut != 1 {
				t.Fatalf("writes that hit their deadline = %d, want 1", timedOut)
			}
			if unarmed != 0 {
				t.Fatalf("%d writes had no fresh write deadline before them: %q", unarmed, frames)
			}
			last := frames[len(frames)-1]
			switch tt.trigger {
			case "event":
				if !strings.Contains(last, `"type":"test.stuck"`) {
					t.Fatalf("blocked frame = %q, want the event", last)
				}
			case "keepalive":
				if last != keepaliveFrame {
					t.Fatalf("blocked frame = %q, want a keepalive", last)
				}
			}
		})
	}
}

// TestServeFiltered_RealClientStopsReading runs the hub behind a real
// net/http server, so the deadline goes through http.ResponseController to
// the TCP connection. The client reads the connected frame and then nothing,
// so once the socket buffers fill, a write blocks until its deadline.
func TestServeFiltered_RealClientStopsReading(t *testing.T) {
	h := New()
	h.keepalive = time.Hour
	h.writeTimeout = 100 * time.Millisecond
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: {\"type\":\"connected\"}\n" {
		t.Fatalf("first line = %q, %v; want the connected frame", line, err)
	}
	waitFor(t, "client to register", func() bool { return clientCount(h) == 1 })

	// 60 MiB in 1 MiB events, far more than loopback socket buffers hold
	// (a few MiB each way), and under the 64-event queue, so none is dropped.
	big := strings.Repeat("x", 1<<20)
	for i := 0; i < 60; i++ {
		h.Publish(domain.Event{Type: domain.EventType("test.big"), ProjectID: 1, Payload: big})
	}

	waitForWithin(t, 8*time.Second, "the stuck stream to be unregistered", func() bool { return clientCount(h) == 0 })
}

func TestWriteTimeoutValue(t *testing.T) {
	tests := []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{name: "configured", set: 5 * time.Millisecond, want: 5 * time.Millisecond},
		{name: "zero falls back to default", set: 0, want: DefaultWriteTimeout},
		{name: "negative falls back to default", set: -time.Second, want: DefaultWriteTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Hub{writeTimeout: tt.set}
			if got := h.writeTimeoutValue(); got != tt.want {
				t.Fatalf("writeTimeoutValue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNew_UsesDefaultWriteTimeout(t *testing.T) {
	if got := New().writeTimeoutValue(); got != 10*time.Second {
		t.Fatalf("default write timeout = %v, want 10s", got)
	}
}

// gatedWriter is a streamRecorder whose writes wait while its gate is shut,
// like a client that pauses reading and later catches up: a write held at
// the gate succeeds once the gate opens. It has no write deadline, so a
// stream it holds ends only through the overflow signal.
type gatedWriter struct {
	*streamRecorder
	shut     atomic.Bool
	waiting  atomic.Int32 // writes held at the gate now
	open     chan struct{}
	openOnce sync.Once
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{streamRecorder: newStreamRecorder(), open: make(chan struct{})}
}

func (g *gatedWriter) Write(p []byte) (int, error) {
	if g.shut.Load() {
		g.waiting.Add(1)
		<-g.open
		g.waiting.Add(-1)
	}
	return g.streamRecorder.Write(p)
}

// release opens the gate for good: held and later writes go through.
func (g *gatedWriter) release() { g.openOnce.Do(func() { close(g.open) }) }

// logRecords is a slog.Handler that keeps every record.
type logRecords struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *logRecords) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecords) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logRecords) WithGroup(string) slog.Handler            { return l }

func (l *logRecords) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r.Clone())
	return nil
}

// warnings counts the warnings logged, or with userID >= 0 only those whose
// user_id is userID.
func (l *logRecords) warnings(userID int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.records {
		if r.Level != slog.LevelWarn {
			continue
		}
		if userID < 0 {
			n++
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "user_id" && a.Value.Kind() == slog.KindInt64 && a.Value.Int64() == int64(userID) {
				n++
				return false
			}
			return true
		})
	}
	return n
}

const anyUser = -1

func TestServeFiltered_FullBufferEndsOnlyThatStream(t *testing.T) {
	h := New()
	h.keepalive = time.Hour // only events are written
	logs := &logRecords{}
	h.log = slog.New(logs)

	const slowUser, healthyUser = 42, 7
	healthy := newStreamRecorder()
	_, healthyDone := serveWriterAs(t, h, healthy, healthyUser, nil, nil)
	slow := newGatedWriter()
	_, slowDone := serveWriterAs(t, h, slow, slowUser, nil, slow.release)

	waitFor(t, "both clients to register", func() bool { return clientCount(h) == 2 })
	waitFor(t, "both connected frames", func() bool {
		_, a := healthy.snapshot()
		_, b := slow.snapshot()
		return a == 1 && b == 1
	})

	const fill = `"type":"test.fill"`
	received := func(rec *streamRecorder, marker string) int {
		out, _ := rec.snapshot()
		return strings.Count(out, marker)
	}
	// publish sends n events and waits for the healthy stream to write them
	// all, so it is never more than n (under the buffer) behind.
	sent := 0
	publish := func(n int) {
		t.Helper()
		for range n {
			h.Publish(domain.Event{Type: domain.EventType("test.fill"), ProjectID: 1})
		}
		sent += n
		waitFor(t, fmt.Sprintf("the healthy stream to write %d events", sent), func() bool { return received(healthy, fill) == sent })
	}

	// The slow client stops reading. Its stream takes the first event and
	// is held writing it, so the next subscriberBuffer events queue.
	slow.shut.Store(true)
	publish(1)
	waitFor(t, "the slow stream to be held writing the first event", func() bool { return slow.waiting.Load() == 1 })
	publish(subscriberBuffer / 2)
	publish(subscriberBuffer / 2)
	if n := logs.warnings(anyUser); n != 0 {
		t.Fatalf("warnings with the slow stream's buffer exactly full = %d, want 0", n)
	}
	select {
	case <-slowDone:
		t.Fatal("the slow stream ended before its buffer overflowed")
	default:
	}

	// One more event overflows it. Overflowing it again must not log again.
	publish(1)
	publish(10)
	if n := logs.warnings(slowUser); n != 1 {
		t.Fatalf("overflow warnings for user %d = %d, want 1", slowUser, n)
	}
	if n := logs.warnings(anyUser); n != 1 {
		t.Fatalf("warnings = %d, want only the slow stream's one", n)
	}

	// The client catches up: its held write completes, and the stream ends
	// without writing any of the events still queued.
	slow.release()
	select {
	case <-slowDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the overflowed stream was still running 2s after its client caught up")
	}
	if got := received(slow.streamRecorder, fill); got != 1 {
		t.Fatalf("events the overflowed stream wrote = %d, want only the 1 it was holding", got)
	}
	if n := clientCount(h); n != 1 {
		t.Fatalf("clients after the overflowed stream ended = %d, want 1 (the healthy one)", n)
	}

	// The healthy stream is still up and still gets events.
	h.Publish(domain.Event{Type: domain.EventType("test.after"), ProjectID: 1})
	waitFor(t, "the healthy stream to get an event after the overflow", func() bool {
		return received(healthy, `"type":"test.after"`) == 1
	})
	select {
	case <-healthyDone:
		t.Fatal("the healthy stream ended")
	default:
	}
}

// TestDeliver_RepeatedOverflowUnderConcurrentPublishers floods stuck
// streams from many publishers (Publish and Deliver) while other streams
// connect and disconnect. Under -race it checks that publishing never
// blocks, that no stream's done is closed twice (that would panic), and that
// each stuck stream warns once.
func TestDeliver_RepeatedOverflowUnderConcurrentPublishers(t *testing.T) {
	h := New()
	h.keepalive = time.Hour
	logs := &logRecords{}
	h.log = slog.New(logs)

	const stuck, firstUser = 4, 100
	slows := make([]*gatedWriter, stuck)
	dones := make([]<-chan struct{}, stuck)
	for i := range stuck {
		slows[i] = newGatedWriter()
		_, dones[i] = serveWriterAs(t, h, slows[i], firstUser+i, nil, slows[i].release)
	}
	waitFor(t, "the stuck streams' connected frames", func() bool {
		for _, s := range slows {
			if _, n := s.snapshot(); n != 1 {
				return false
			}
		}
		return true
	})
	for _, s := range slows {
		s.shut.Store(true)
	}

	// Streams that come and go while events flow, so registering and
	// unregistering race with delivery and overflow.
	stopChurn := make(chan struct{})
	var churn sync.WaitGroup
	for range 2 {
		churn.Go(func() {
			for {
				select {
				case <-stopChurn:
					return
				default:
				}
				ctx, cancel := context.WithCancel(context.Background())
				req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
				served := make(chan struct{})
				go func() {
					defer close(served)
					h.ServeFiltered(newStreamRecorder(), req, 0, nil)
				}()
				time.Sleep(time.Millisecond)
				cancel()
				<-served
			}
		})
	}

	const publishers, perPublisher = 8, 500
	var pub sync.WaitGroup
	for p := range publishers {
		pub.Go(func() {
			for i := range perPublisher {
				evt := domain.Event{Type: domain.EventType("test.flood"), ProjectID: 1}
				if (p+i)%2 == 0 {
					h.Publish(evt)
				} else {
					h.Deliver(evt)
				}
			}
		})
	}
	published := make(chan struct{})
	go func() {
		pub.Wait()
		close(published)
	}()
	select {
	case <-published:
	case <-time.After(10 * time.Second):
		t.Fatal("publishing blocked behind the stuck streams")
	}
	close(stopChurn)
	churn.Wait()

	for i := range stuck {
		if n := logs.warnings(firstUser + i); n != 1 {
			t.Fatalf("overflow warnings for stuck user %d = %d, want 1", firstUser+i, n)
		}
	}
	for i, s := range slows {
		s.release()
		select {
		case <-dones[i]:
		case <-time.After(2 * time.Second):
			t.Fatalf("stuck stream %d still running 2s after its client caught up", i)
		}
	}
	if n := clientCount(h); n != 0 {
		t.Fatalf("clients left = %d, want 0", n)
	}
}
