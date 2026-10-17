// Package hub provides the SSE event hub.
// It lives in its own package to break the import cycle between api and actor.
package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/simstech/amp-api/internal/domain"
)

// Hub manages all live SSE connections and broadcasts events to them.
// Actors call hub.Publish() after every state change — the hub fans out
// to every connected browser client instantly, no polling needed.
type Hub struct {
	mu      sync.RWMutex
	clients map[string]chan domain.Event
	sinks   []func(domain.Event)
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
		clients: make(map[string]chan domain.Event),
	}
}

// Publish sends an event to all connected SSE clients.
// Non-blocking: drops slow clients rather than stalling actors.
func (h *Hub) Publish(evt domain.Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, fn := range h.sinks {
		fn(evt)
	}
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
func (h *Hub) ServeFiltered(w http.ResponseWriter, r *http.Request, allow Allow) {
	flusher, ok := w.(http.Flusher)
	if !ok {
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

	fmt.Fprintf(w, "data: {\"type\":\"connected\"}\n\n")
	flusher.Flush()

	for {
		select {
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
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
