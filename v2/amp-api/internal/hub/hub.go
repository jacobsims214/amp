// Package hub provides the SSE event hub.
// It lives in its own package to break the import cycle between api and actor.
package hub

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// DefaultKeepalive is how often an open SSE stream gets a comment line when
// nothing else is sent. It must stay under the load balancer's idle timeout
// (60 s on the AWS ALB), or idle streams are cut.
const DefaultKeepalive = 25 * time.Second

// keepaliveFrame is an SSE comment: EventSource ignores it, but it keeps
// bytes moving on an idle connection.
const keepaliveFrame = ": keepalive\n\n"

// DefaultWriteTimeout bounds one write to an SSE stream, flush included.
// The API server sets no WriteTimeout, so without it a client that stops
// reading (a frozen or sleeping tab) blocks the write, and with it the
// stream's goroutine and its proxy buffer, until TCP gives up. When the
// deadline passes, the write fails and the stream ends.
const DefaultWriteTimeout = 10 * time.Second

// subscriberBuffer is how many events a stream can fall behind by. One
// more, and the stream is ended (see deliver).
const subscriberBuffer = 64

// subscriber is one live SSE stream.
type subscriber struct {
	events chan domain.Event
	// done is closed, once, when events overflowed: the stream has missed
	// an event, so ServeFiltered ends it and the browser reconnects and
	// resyncs. Only deliver closes it, after winning overflowed.
	done       chan struct{}
	overflowed atomic.Bool
	// userID is who the stream is for, for the overflow warning; 0 when
	// unknown (the unfiltered ServeHTTP, or the local-dev user before it is
	// provisioned).
	userID int
}

// overflow marks s as having missed an event and signals its stream to
// end. It reports whether this call did it, so only the first overflow is
// logged. It never blocks and is safe to call concurrently.
func (s *subscriber) overflow() bool {
	if !s.overflowed.CompareAndSwap(false, true) {
		return false
	}
	close(s.done)
	return true
}

// Hub manages all live SSE connections and broadcasts events to them.
// Actors call hub.Publish() after every state change — the hub fans out
// to every connected browser client instantly, no polling needed.
type Hub struct {
	mu      sync.RWMutex
	clients map[*subscriber]struct{}
	sinks   []func(domain.Event)
	relay   func(domain.Event)

	// keepalive is the interval between keepalive comments on each stream.
	// New sets it to DefaultKeepalive; tests in this package shorten it.
	keepalive time.Duration

	// writeTimeout bounds each write to a stream. New sets it to
	// DefaultWriteTimeout; tests in this package shorten it.
	writeTimeout time.Duration

	// log receives the overflow warning; nil means slog.Default() at the
	// time of the warning. Tests in this package set it to capture records.
	log *slog.Logger
}

// SetRelay sends every event published here on to the other replicas
// (internal/cluster). fn runs inside Publish, so it must not block.
func (h *Hub) SetRelay(fn func(domain.Event)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.relay = fn
}

// AddSink registers fn to receive every published event (webhooks use
// this). fn runs inside Publish, on the publishing actor, so it must not
// block.
func (h *Hub) AddSink(fn func(domain.Event)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sinks = append(h.sinks, fn)
}

func New() *Hub {
	return &Hub{
		clients:      make(map[*subscriber]struct{}),
		keepalive:    DefaultKeepalive,
		writeTimeout: DefaultWriteTimeout,
	}
}

// writeTimeoutValue is the configured per-write bound, or
// DefaultWriteTimeout when it is unset or not positive, so a Hub never
// writes without one.
func (h *Hub) writeTimeoutValue() time.Duration {
	if h.writeTimeout <= 0 {
		return DefaultWriteTimeout
	}
	return h.writeTimeout
}

func (h *Hub) logger() *slog.Logger {
	if h.log == nil {
		return slog.Default()
	}
	return h.log
}

// keepaliveInterval is the configured interval, or DefaultKeepalive when it
// is unset or not positive (time.NewTicker panics on those).
func (h *Hub) keepaliveInterval() time.Duration {
	if h.keepalive <= 0 {
		return DefaultKeepalive
	}
	return h.keepalive
}

// Publish sends an event to all connected SSE clients, the sinks, and the
// other replicas.
// Non-blocking: a client too far behind to take the event has its stream
// ended (see deliver) rather than stalling actors.
func (h *Hub) Publish(evt domain.Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, fn := range h.sinks {
		fn(evt)
	}
	if h.relay != nil {
		h.relay(evt)
	}
	h.deliver(evt)
}

// Deliver sends an event that another replica published to this replica's
// SSE clients only: that replica already ran the sinks.
func (h *Hub) Deliver(evt domain.Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	h.deliver(evt)
}

// deliver queues evt on every stream. The caller holds h.mu's read lock.
//
// A stream whose queue is full can't take evt without blocking every
// publisher, so it misses it. A stream that missed an event shows a stale
// board until it resyncs, so deliver ends it instead: it signals the
// stream's done (never closing its events channel, which ServeFiltered
// owns), and logs that once. The browser's EventSource reconnects, the new
// stream's connected frame makes the page refetch, and the board is current
// again.
func (h *Hub) deliver(evt domain.Event) {
	for s := range h.clients {
		select {
		case s.events <- evt:
		default:
			if s.overflow() {
				h.logger().Warn("SSE stream fell behind: its event buffer is full, so it is being ended to make the browser reconnect and resync",
					"user_id", s.userID, "buffer", cap(s.events))
			}
		}
	}
}

// ServeHTTP is the SSE endpoint with no filtering (every event) and no
// known user.
// GET /api/events
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.ServeFiltered(w, r, 0, nil)
}

// Allow decides whether a connection may see events for a project. It runs
// on the connection's own goroutine (never inside Publish), so it may block.
type Allow func(projectID int) bool

// ServeFiltered is ServeHTTP for userID's connection, dropping events that
// user can't see. A nil allow passes everything. userID only labels the
// stream in logs; 0 means unknown.
//
// Every write to the stream (the connected frame, keepalives and events)
// runs under a fresh write deadline. A write or flush that fails, whether
// the client went away or stopped reading and the deadline passed, ends the
// stream and unregisters it. So does falling a full buffer of events behind
// (see deliver): the browser then reconnects and resyncs.
func (h *Hub) ServeFiltered(w http.ResponseWriter, r *http.Request, userID int, allow Allow) {
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	sub := &subscriber{
		events: make(chan domain.Event, subscriberBuffer),
		done:   make(chan struct{}),
		userID: userID,
	}
	h.mu.Lock()
	h.clients[sub] = struct{}{}
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.clients, sub)
		h.mu.Unlock()
		close(sub.events)
	}()

	rc := http.NewResponseController(w)
	timeout := h.writeTimeoutValue()
	// send writes one frame and flushes it under a fresh write deadline. The
	// deadline is set on the connection, so it bounds the flush too, which
	// is where a buffered write actually blocks. As in internal/relay, a
	// writer without deadlines (ErrNotSupported: a test recorder) is written
	// without one.
	send := func(frame string) error {
		_ = rc.SetWriteDeadline(time.Now().Add(timeout))
		if _, err := io.WriteString(w, frame); err != nil {
			return err
		}
		return rc.Flush()
	}

	if err := send("data: {\"type\":\"connected\"}\n\n"); err != nil {
		return
	}

	// Send a comment on a fixed interval so an idle stream isn't cut by the
	// load balancer's idle timeout. Stopped when the connection ends.
	keepalive := time.NewTicker(h.keepaliveInterval())
	defer keepalive.Stop()

	for {
		// An overflowed stream ends before it writes anything more, even
		// with events still queued: the client resyncs when it reconnects.
		select {
		case <-sub.done:
			return
		default:
		}
		select {
		case <-sub.done:
			return // fell a full buffer behind; the client reconnects
		case <-keepalive.C:
			if err := send(keepaliveFrame); err != nil {
				return // the client is gone or stopped reading
			}
		case evt, ok := <-sub.events:
			if !ok {
				return
			}
			if allow != nil && !allow(evt.ProjectID) {
				continue
			}
			data, err := json.Marshal(evt)
			if err != nil {
				continue
			}
			if err := send("data: " + string(data) + "\n\n"); err != nil {
				return // the client is gone or stopped reading
			}
		case <-r.Context().Done():
			return
		}
	}
}
