package relay

import (
	"net/http"
	"sync"

	"github.com/simstech/amp-api/internal/domain"
)

// Registry holds the live state of the sessions this replica serves. With one
// replica (M1) every session is local: LocalRegistry. The multi-replica work
// (#2377) keeps this interface and adds a Forwarder that sends a browser call
// to the replica named in remote_sessions.holder_node.
type Registry interface {
	// Get returns the session's live state on this replica, nil if none.
	Get(id string) *Live
	// Ensure returns the session's live state, creating it (empty buffer,
	// harness offline) if this replica has none.
	Ensure(sess *domain.RemoteSession) *Live
	// Remove forgets the session and returns what was held, nil if nothing.
	Remove(id string) *Live
	// All lists the live states held here.
	All() []*Live
}

// Forwarder serves a browser call for a session whose harness stream another
// replica holds. It returns false when the call should be served here. M1
// runs one replica, so the default (nil) serves everything locally.
type Forwarder interface {
	Forward(w http.ResponseWriter, r *http.Request, sess *domain.RemoteSession) bool
}

// LocalRegistry is the single-replica Registry: a map under a mutex.
type LocalRegistry struct {
	mu       sync.Mutex
	sessions map[string]*Live
}

// NewLocalRegistry returns an empty LocalRegistry.
func NewLocalRegistry() *LocalRegistry { return &LocalRegistry{sessions: map[string]*Live{}} }

func (r *LocalRegistry) Get(id string) *Live {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

func (r *LocalRegistry) Ensure(sess *domain.RemoteSession) *Live {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.sessions[sess.ID]; ok && !l.Ended() {
		return l
	}
	l := newLive(sess)
	r.sessions[sess.ID] = l
	return l
}

func (r *LocalRegistry) Remove(id string) *Live {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.sessions[id]
	delete(r.sessions, id)
	return l
}

func (r *LocalRegistry) All() []*Live {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Live, 0, len(r.sessions))
	for _, l := range r.sessions {
		out = append(out, l)
	}
	return out
}
