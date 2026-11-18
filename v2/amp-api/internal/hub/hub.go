// Package hub provides the SSE event hub.
// It lives in its own package to break the import cycle between api and actor.
package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
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

// Hub manages all live SSE connections and broadcasts events to them.
// Actors call hub.Publish() after every state change — the hub fans out
// to every connected browser client instantly, no polling needed.
type Hub struct {
	mu      sync.RWMutex
	clients map[string]chan domain.Event
	sinks   []func(domain.Event)
	relay   func(domain.Event)

	// keepalive is the interval between keepalive comments on each stream.
	// New sets it to DefaultKeepalive; tests in this package shorten it.
	keepalive time.Duration

	// writeTimeout bounds each write to a stream. New sets it to
	// DefaultWriteTimeout; tests in this package shorten it.
	writeTimeout time.Duration
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
		clients:      make(map[string]chan domain.Event),
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
// Non-blocking: drops slow clients rather than stalling actors.
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

func (h *Hub) deliver(evt domain.Event) {
	for _, ch := range h.clients {
		select {
		case ch <- evt:
		default:
			// client is slow — skip, don't block
		}
	}
}

// ServeHTTP is the SSE endpoint with no filtering (every event).
// GET /api/events
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.ServeFiltered(w, r, nil)
}

// Allow decides whether a connection may see events for a project. It runs
// on the connection's own goroutine (never inside Publish), so it may block.
type Allow func(projectID int) bool

// ServeFiltered is ServeHTTP that drops events the connection's user can't
// see. A nil allow passes everything.
//
// Every write to the stream (the connected frame, keepalives and events)
// runs under a fresh write deadline. A write or flush that fails, whether
// the client went away or stopped reading and the deadline passed, ends the
// stream and unregisters it.
func (h *Hub) ServeFiltered(w http.ResponseWriter, r *http.Request, allow Allow) {
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	clientID := fmt.Sprintf("%d", time.Now().UnixNano())
	ch := make(chan domain.Event, 64)
	h.mu.Lock()
	h.clients[clientID] = ch
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.clients, clientID)
		h.mu.Unlock()
		close(ch)
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
		select {
		case <-keepalive.C:
			if err := send(keepaliveFrame); err != nil {
				return // the client is gone or stopped reading
			}
		case evt, ok := <-ch:
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
