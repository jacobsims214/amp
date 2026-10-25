package relay

// The in-memory state of one live session on the replica that holds its
// harness connection: the harness's command stream, the viewers' event
// streams, the resync buffer, the pairing window and the rate limiters.
// Nothing here is ever written to the database or the logs (decision D3).

import (
	"crypto/ecdsa"
	"encoding/json"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// Buffer and stream limits (protocol.md sections 10 and 11).
const (
	BufferEvents    = 200
	BufferBytes     = 2 << 20
	BufferTTL       = 10 * time.Minute
	MaxViewers      = 3
	PairingTries    = 5
	PairingWindow   = 5 * time.Minute
	viewerQueue     = 512 // frames queued for a slow viewer before it's dropped
	harnessQueue    = 64  // frames queued for the harness before commands are refused
	refusedAuditGap = time.Minute

	// ViewerQueueBytes bounds the bytes queued for one viewer (#2390): a
	// viewer that falls this far behind is dropped and resumes from the
	// buffer with Last-Event-ID. Frames share their data with the buffer,
	// but once the buffer evicts an event only the queues hold it.
	ViewerQueueBytes = 4 << 20
	// DefaultQueuedBytes bounds the bytes queued for all viewers on one
	// replica (Config.MaxQueuedBytes), counting a frame once per viewer.
	DefaultQueuedBytes = 64 << 20
	// EventBytesPerSec and EventBytesBurst limit the event bytes one
	// session's harness posts (#2390): 2 MiB/s, burst 8 MiB, else 429.
	EventBytesPerSec = 2 << 20
	EventBytesBurst  = 8 << 20
)

// frame is one SSE frame: an optional id, an event name and one data line.
type frame struct {
	id    int64 // 0: no id: line
	event string
	data  []byte
}

// size is what a queued frame counts against the byte bounds.
func (f frame) size() int64 { return int64(len(f.data) + len(f.event) + 32) }

// byteBudget is a byte bound shared by several streams.
type byteBudget struct {
	max  int64
	used atomic.Int64
}

func newByteBudget(max int64) *byteBudget { return &byteBudget{max: max} }

// reserve takes n bytes; false (and nothing taken) when that would pass max.
func (b *byteBudget) reserve(n int64) bool {
	if b == nil {
		return true
	}
	if b.used.Add(n) > b.max {
		b.used.Add(-n)
		return false
	}
	return true
}

func (b *byteBudget) release(n int64) {
	if b != nil {
		b.used.Add(-n)
	}
}

// Used is the bytes currently reserved.
func (b *byteBudget) Used() int64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}

// stream is one open SSE response (the harness's command stream or a
// viewer's event stream). Frames are queued without blocking; close ends it.
// A viewer's queue is bounded by bytes as well as frames: its own bound and
// the replica-wide one.
type stream struct {
	ch     chan frame
	done   chan struct{}
	once   sync.Once
	want   int64       // a viewer's: the seq it asked to resume from (0: none)
	own    *byteBudget // nil: no byte bound
	shared *byteBudget // nil: no byte bound
}

func newStream(n int) *stream { return &stream{ch: make(chan frame, n), done: make(chan struct{})} }

// newViewerStream is a viewer's stream: 512 frames, ViewerQueueBytes, and
// the replica's shared bound.
func newViewerStream(shared *byteBudget) *stream {
	st := newStream(viewerQueue)
	st.own, st.shared = newByteBudget(ViewerQueueBytes), shared
	return st
}

// send queues f; false when the stream is closed or its queue is full (by
// frames or by bytes).
func (s *stream) send(f frame) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	n := f.size()
	if !s.own.reserve(n) {
		return false
	}
	if !s.shared.reserve(n) {
		s.own.release(n)
		return false
	}
	select {
	case s.ch <- f:
		return true
	default:
		s.own.release(n)
		s.shared.release(n)
		return false
	}
}

// dequeued releases a frame taken off the queue.
func (s *stream) dequeued(f frame) {
	n := f.size()
	s.own.release(n)
	s.shared.release(n)
}

// close ends the stream after the frames already queued.
func (s *stream) close() { s.once.Do(func() { close(s.done) }) }

// drain drops what is still queued and releases its bytes. Call it once
// nothing sends to the stream any more (it is out of the session's maps).
func (s *stream) drain() {
	for {
		select {
		case f := <-s.ch:
			s.dequeued(f)
		default:
			return
		}
	}
}

// Queued is the bytes queued for the stream.
func (s *stream) Queued() int64 { return s.own.Used() }

type bufferedEvent struct {
	seq  int64
	data []byte
	at   time.Time
}

// pairingWindow is the window the harness opened with POST .../pairings.
type pairingWindow struct {
	id       string
	expires  time.Time
	attempts int
}

// pendingPair is what the relay remembers of a forwarded pair request, so it
// can record the device when the harness's signed pair_result accepts it.
type pendingPair struct {
	pairingID string
	devicePub string
	name      string
}

// bucket is a token bucket: rate tokens per minute, up to burst.
type bucket struct {
	perMin float64
	burst  float64
	tokens float64
	at     time.Time
}

func newBucket(perMin, burst int) *bucket {
	return &bucket{perMin: float64(perMin), burst: float64(burst), tokens: float64(burst)}
}

// take spends a token; when none is left it returns how long until one is.
func (b *bucket) take(now time.Time) (bool, time.Duration) { return b.takeN(now, 1) }

// takeN spends n tokens; when fewer are left it returns how long until
// there are n (n must be at most the burst).
func (b *bucket) takeN(now time.Time, n float64) (bool, time.Duration) {
	if !b.at.IsZero() {
		b.tokens = math.Min(b.burst, b.tokens+now.Sub(b.at).Minutes()*b.perMin)
	}
	b.at = now
	if b.tokens >= n {
		b.tokens -= n
		return true, 0
	}
	wait := time.Duration((n - b.tokens) / b.perMin * float64(time.Minute))
	return false, wait
}

// Live is one session's in-memory state on this replica.
type Live struct {
	ID          string
	OwnerUserID int
	ProjectID   int
	HarnessID   string
	HarnessPub  string
	harnessKey  *ecdsa.PublicKey

	mu        sync.Mutex
	harness   *stream
	heartbeat time.Time
	online    bool // the presence last announced to viewers
	viewers   map[*stream]struct{}

	ring            []bufferedEvent
	ringBytes       int
	acceptedThrough int64 // the last event seq accepted; 0 before the first
	hello, status   []byte

	pairing  *pairingWindow
	pending  map[string]pendingPair // device_id -> the forwarded request
	devices  string                 // the last status.devices set reconciled
	meta     string                 // the last status model/ticket stored (#2389)
	prompts  *bucket                // 20/min, burst 5
	ops      *bucket                // 120/min
	attaches *bucket                // 20/min
	events   *bucket                // event bytes: 2 MiB/s, burst 8 MiB
	refused  map[string]time.Time   // the last remote.refused audit per code

	ended bool
}

func newLive(sess *domain.RemoteSession) *Live {
	key, _ := parsePub(sess.HarnessPub)
	return &Live{
		ID: sess.ID, OwnerUserID: sess.OwnerUserID, ProjectID: sess.ProjectID,
		HarnessID: sess.HarnessID, HarnessPub: sess.HarnessPub, harnessKey: key,
		viewers:  map[*stream]struct{}{},
		pending:  map[string]pendingPair{},
		prompts:  newBucket(20, 5),
		ops:      newBucket(120, 20),
		attaches: newBucket(20, 10),
		events:   newBucket(EventBytesPerSec*60, EventBytesBurst),
		refused:  map[string]time.Time{},
	}
}

// relayFrame is an unsigned status frame from AMP to viewers (protocol 8.3).
func relayFrame(v map[string]any) frame {
	b, _ := json.Marshal(v)
	return frame{event: "relay", data: b}
}

// isOnline: the command stream is open and a heartbeat arrived recently.
func (l *Live) isOnline(now time.Time, offlineAfter time.Duration) bool {
	return l.harness != nil && now.Sub(l.heartbeat) <= offlineAfter
}

// Online reports the harness's presence.
func (l *Live) Online(now time.Time, offlineAfter time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.isOnline(now, offlineAfter)
}

// Viewers is the number of open event streams.
func (l *Live) Viewers() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.viewers)
}

// updatePresence tells viewers when the presence changed. Call with mu held.
func (l *Live) updatePresence(now time.Time, offlineAfter time.Duration) {
	on := l.isOnline(now, offlineAfter)
	if on == l.online {
		return
	}
	l.online = on
	t := "offline"
	if on {
		t = "online"
	}
	l.broadcast(relayFrame(map[string]any{"type": t}))
}

// broadcast queues f for every viewer, dropping viewers that can't keep up
// (they reconnect with Last-Event-ID). Call with mu held.
func (l *Live) broadcast(f frame) {
	dropped := false
	for v := range l.viewers {
		if !v.send(f) {
			v.close()
			delete(l.viewers, v)
			dropped = true
		}
	}
	if dropped {
		l.tellHarnessViewers()
	}
}

// tellHarnessViewers sends the viewer count to the harness. Call with mu held.
func (l *Live) tellHarnessViewers() {
	if l.harness != nil {
		b, _ := json.Marshal(map[string]int{"count": len(l.viewers)})
		l.harness.send(frame{event: "viewers", data: b})
	}
}

// evict drops buffered events past the TTL and over the size limits.
// Call with mu held.
func (l *Live) evict(now time.Time) {
	n := 0
	for n < len(l.ring) {
		e := l.ring[n]
		if now.Sub(e.at) <= BufferTTL && len(l.ring)-n <= BufferEvents && l.ringBytes <= BufferBytes {
			break
		}
		l.ringBytes -= len(e.data)
		n++
	}
	if n > 0 {
		l.ring = append([]bufferedEvent(nil), l.ring[n:]...)
	}
}

// oldest is the oldest buffered seq (0 when the buffer is empty).
func (l *Live) oldest() int64 {
	if len(l.ring) == 0 {
		return 0
	}
	return l.ring[0].seq
}

// end closes every stream of the session: the harness gets `end`, viewers a
// relay `ended` frame. The buffer is dropped.
func (l *Live) end(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return
	}
	l.ended = true
	if l.harness != nil {
		b, _ := json.Marshal(map[string]string{"reason": reason})
		l.harness.send(frame{event: "end", data: b})
		l.harness.close()
		l.harness = nil
	}
	l.broadcast(relayFrame(map[string]any{"type": "ended", "reason": reason}))
	for v := range l.viewers {
		v.close()
	}
	l.viewers = map[*stream]struct{}{}
	l.ring, l.ringBytes, l.hello, l.status = nil, 0, nil, nil
	l.pending = map[string]pendingPair{}
}

// release forgets the session here without ending it (another replica took
// it over): the streams close with no end frame, so the harness and the
// viewers reconnect and are passed to the new holder.
func (l *Live) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return
	}
	l.ended = true
	if l.harness != nil {
		l.harness.close()
		l.harness = nil
	}
	for v := range l.viewers {
		v.close()
	}
	l.viewers = map[*stream]struct{}{}
	l.ring, l.ringBytes, l.hello, l.status = nil, 0, nil, nil
	l.pending = map[string]pendingPair{}
}

// Ended reports whether the session ended while this replica held it.
func (l *Live) Ended() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ended
}
