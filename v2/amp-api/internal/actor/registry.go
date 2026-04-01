package actor

import (
	"fmt"
	"sync"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/simstech/amp-api/internal/hub"
	"github.com/simstech/amp-api/internal/repository"
)

// Registry maps project IDs to their running ProjectActor PIDs.
// It creates actors on first access (lazy init) and keeps them alive
// for the lifetime of the process — the supervisor restarts any that crash.
type Registry struct {
	mu     sync.RWMutex
	system *actor.ActorSystem
	actors map[int]*actor.PID
	repo   *repository.Repo
	hub    *hub.Hub
}

func NewRegistry(system *actor.ActorSystem, repo *repository.Repo, h *hub.Hub) *Registry {
	return &Registry{
		system: system,
		actors: make(map[int]*actor.PID),
		repo:   repo,
		hub:    h,
	}
}

// System exposes the underlying ActorSystem for callers that need to Send messages directly.
func (r *Registry) System() *actor.ActorSystem { return r.system }

// Get returns the PID for a project actor, creating it if it doesn't exist.
func (r *Registry) Get(projectID int) (*actor.PID, error) {
	r.mu.RLock()
	pid, ok := r.actors[projectID]
	r.mu.RUnlock()
	if ok {
		return pid, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock.
	if pid, ok = r.actors[projectID]; ok {
		return pid, nil
	}

	props := actor.PropsFromProducer(func() actor.Actor {
		return NewProjectActor(projectID, r.repo, r.hub)
	})

	pid, err := r.system.Root.SpawnNamed(props, fmt.Sprintf("project-%d", projectID))
	if err != nil {
		return nil, fmt.Errorf("spawn project actor %d: %w", projectID, err)
	}

	r.actors[projectID] = pid
	return pid, nil
}

// Send delivers a message to the project actor and returns immediately.
// Use this for fire-and-forget. For request/reply use Ask().
func (r *Registry) Send(projectID int, msg interface{}) error {
	pid, err := r.Get(projectID)
	if err != nil {
		return err
	}
	r.system.Root.Send(pid, msg)
	return nil
}
