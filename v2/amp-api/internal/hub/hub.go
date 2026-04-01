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
	for _, ch := range h.clients {
		select {
		case ch <- evt:
		default:
			// client is slow — skip, don't block
		}
	}
}

// ServeHTTP is the SSE endpoint.
// GET /api/events
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
